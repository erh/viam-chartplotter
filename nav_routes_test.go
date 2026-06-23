package vc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeMetaStore is an in-memory metadataStore that JSON-round-trips on every
// read and write, mimicking the structpb wire conversion the real app client
// does (Go structs become plain maps, ints become float64). This catches code
// that relies on shared references or concrete Go types surviving a round trip.
type fakeMetaStore struct {
	meta map[string]interface{}
}

func jsonRoundTrip(m map[string]interface{}) map[string]interface{} {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	if out == nil {
		out = map[string]interface{}{}
	}
	return out
}

func newFakeMetaStore(initial map[string]interface{}) *fakeMetaStore {
	if initial == nil {
		initial = map[string]interface{}{}
	}
	return &fakeMetaStore{meta: jsonRoundTrip(initial)}
}

func (f *fakeMetaStore) Get(_ context.Context) (map[string]interface{}, error) {
	return jsonRoundTrip(f.meta), nil
}

func (f *fakeMetaStore) Update(_ context.Context, meta map[string]interface{}) error {
	f.meta = jsonRoundTrip(meta)
	return nil
}

func mkRouteRec() routeRec {
	const now = "2026-06-19T00:00:00Z"
	return routeRec{
		ID:        "rte_a",
		Name:      "Test",
		Source:    "manual",
		Color:     "#ff8800",
		CreatedAt: now,
		UpdatedAt: now,
		Waypoints: []routeWaypoint{{Lat: 41.1, Lng: -71.5}, {Lat: 41.2, Lng: -71.4}},
	}
}

func TestRoutesListEmpty(t *testing.T) {
	ctx := context.Background()
	got, err := routesList(ctx, newFakeMetaStore(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 routes, got %d", len(got))
	}
}

func TestRoutesSaveInsertAndStats(t *testing.T) {
	ctx := context.Background()
	store := newFakeMetaStore(nil)
	if err := routesSave(ctx, store, mkRouteRec()); err != nil {
		t.Fatal(err)
	}
	got, err := routesList(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 route, got %d", len(got))
	}
	if got[0].Name != "Test" {
		t.Errorf("name = %q", got[0].Name)
	}
	if got[0].Stats == nil || got[0].Stats.Count != 2 {
		t.Errorf("stats = %+v, want count 2", got[0].Stats)
	}
	if got[0].Stats.DistanceNm <= 0 {
		t.Errorf("distance = %v, want > 0", got[0].Stats.DistanceNm)
	}
}

func TestRoutesSaveUpsert(t *testing.T) {
	ctx := context.Background()
	store := newFakeMetaStore(nil)
	r := mkRouteRec()
	r.Name = "First"
	if err := routesSave(ctx, store, r); err != nil {
		t.Fatal(err)
	}
	r.Name = "Second"
	if err := routesSave(ctx, store, r); err != nil {
		t.Fatal(err)
	}
	got, err := routesList(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 route after upsert, got %d", len(got))
	}
	if got[0].Name != "Second" {
		t.Errorf("name = %q, want Second", got[0].Name)
	}
}

func TestRoutesPreservesForeignKeys(t *testing.T) {
	ctx := context.Background()
	store := newFakeMetaStore(map[string]interface{}{
		"other_tool": map[string]interface{}{"keep": true},
		"scalar":     7,
	})
	if err := routesSave(ctx, store, mkRouteRec()); err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Get(ctx)
	if _, ok := meta["other_tool"]; !ok {
		t.Error("foreign key other_tool was dropped")
	}
	if meta["scalar"].(float64) != 7 {
		t.Errorf("foreign key scalar = %v, want 7", meta["scalar"])
	}
	if _, ok := meta[routesKey]; !ok {
		t.Error("routes key missing after save")
	}
}

func TestRoutesSaveValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		mutfn func(r *routeRec)
	}{
		{"missing id", func(r *routeRec) { r.ID = "" }},
		{"missing name", func(r *routeRec) { r.Name = "" }},
		{"too few waypoints", func(r *routeRec) { r.Waypoints = r.Waypoints[:1] }},
		{"lat out of range", func(r *routeRec) { r.Waypoints[0].Lat = 91 }},
		{"lng out of range", func(r *routeRec) { r.Waypoints[1].Lng = 181 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mkRouteRec()
			tc.mutfn(&r)
			if err := routesSave(ctx, newFakeMetaStore(nil), r); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestRoutesSaveSizeLimit(t *testing.T) {
	ctx := context.Background()
	r := mkRouteRec()
	r.Waypoints = make([]routeWaypoint, 500000)
	for i := range r.Waypoints {
		r.Waypoints[i] = routeWaypoint{Lat: 41.123456, Lng: -71.123456}
	}
	err := routesSave(ctx, newFakeMetaStore(nil), r)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestRoutesDelete(t *testing.T) {
	ctx := context.Background()
	store := newFakeMetaStore(nil)
	a := mkRouteRec()
	a.ID = "a"
	b := mkRouteRec()
	b.ID = "b"
	if err := routesSave(ctx, store, a); err != nil {
		t.Fatal(err)
	}
	if err := routesSave(ctx, store, b); err != nil {
		t.Fatal(err)
	}
	if err := routesDelete(ctx, store, "a"); err != nil {
		t.Fatal(err)
	}
	got, _ := routesList(ctx, store)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("after delete got %+v, want only b", got)
	}
	// Unknown id is a no-op, not an error.
	if err := routesDelete(ctx, store, "nope"); err != nil {
		t.Fatal(err)
	}
	got, _ = routesList(ctx, store)
	if len(got) != 1 {
		t.Fatalf("unknown-id delete changed the list: %+v", got)
	}
}

func TestRoutesRename(t *testing.T) {
	ctx := context.Background()
	store := newFakeMetaStore(nil)
	orig := mkRouteRec()
	orig.ID = "a"
	if err := routesSave(ctx, store, orig); err != nil {
		t.Fatal(err)
	}
	err := routesRename(ctx, store, "a", map[string]interface{}{
		"name":      "Renamed",
		"notes":     "hi",
		"color":     "#123456",
		"updatedAt": "2026-07-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := routesList(ctx, store)
	r := got[0]
	if r.Name != "Renamed" || r.Notes != "hi" || r.Color != "#123456" {
		t.Errorf("rename fields wrong: %+v", r)
	}
	if r.UpdatedAt != "2026-07-01T00:00:00Z" {
		t.Errorf("updatedAt = %q", r.UpdatedAt)
	}
	if len(r.Waypoints) != 2 || r.Waypoints[0].Lat != 41.1 {
		t.Errorf("geometry changed during rename: %+v", r.Waypoints)
	}
}

func TestRoutesRenameUnknown(t *testing.T) {
	ctx := context.Background()
	err := routesRename(ctx, newFakeMetaStore(nil), "ghost", map[string]interface{}{"name": "x"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestRoutesSchemaGuard(t *testing.T) {
	ctx := context.Background()
	store := newFakeMetaStore(map[string]interface{}{
		routesKey: map[string]interface{}{"schemaVersion": 999, "routes": []interface{}{}},
	})
	if err := routesSave(ctx, store, mkRouteRec()); err == nil ||
		!strings.Contains(err.Error(), "newer") {
		t.Fatalf("expected newer-schema error, got %v", err)
	}
}
