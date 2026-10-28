package vc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/navigation"
)

// newTestNav builds a navService wired to an injected location store (or a
// location error to simulate "no access").
func newTestNav(t *testing.T, locStore metadataStore, locErr error) *navService {
	t.Helper()
	svc := &navService{logger: logging.NewTestLogger(t)}
	switch {
	case locErr != nil:
		svc.routesStoreFn = func(_ context.Context) (metadataStore, error) { return nil, locErr }
	case locStore != nil:
		svc.routesStoreFn = func(_ context.Context) (metadataStore, error) { return locStore, nil }
	}
	return svc
}

// withParent makes par the service's only ancestor location.
func withParent(svc *navService, par metadataStore) {
	svc.parentStoresFn = func(_ context.Context) ([]metadataStore, error) {
		return []metadataStore{par}, nil
	}
}

func addTestRoute(t *testing.T, svc *navService, name string, extra map[string]interface{}) {
	t.Helper()
	if _, err := svc.AddRoute(context.Background(), navigation.AddRouteReq{
		Name:      name,
		Waypoints: inlineWaypoints(navPoint{Lat: 41.1, Lng: -71.5}, navPoint{Lat: 41.2, Lng: -71.4}),
		Extra:     extra,
	}); err != nil {
		t.Fatalf("AddRoute %q: %v", name, err)
	}
}

func routesMeta(t *testing.T, svc *navService) []map[string]interface{} {
	t.Helper()
	out, err := svc.DoCommand(context.Background(), map[string]interface{}{"routes_meta": true})
	if err != nil {
		t.Fatalf("routes_meta: %v", err)
	}
	var metas []map[string]interface{}
	for _, r := range out["routes"].([]interface{}) {
		metas = append(metas, r.(map[string]interface{}))
	}
	return metas
}

func TestAddRouteUsesLocation(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	addTestRoute(t, svc, "Run", map[string]interface{}{"notes": "n", "color": "#123456", "source": "track"})
	recs, _ := routesList(ctx, loc)
	if len(recs) != 1 {
		t.Fatalf("location should hold the route, got %d", len(recs))
	}
	r := recs[0]
	if r.Notes != "n" || r.Color != "#123456" || r.Source != "track" || r.CreatedAt == "" || r.UpdatedAt == "" {
		t.Fatalf("stored record: %+v", r)
	}
}

func TestAddRouteErrorsWhenLocationUnavailable(t *testing.T) {
	svc := newTestNav(t, nil, errors.New("no location access"))
	_, err := svc.AddRoute(context.Background(), navigation.AddRouteReq{
		Name:      "Run",
		Waypoints: inlineWaypoints(navPoint{Lat: 1, Lng: 1}, navPoint{Lat: 2, Lng: 2}),
	})
	if err == nil {
		t.Fatal("expected error when the location can't be written")
	}
}

func TestRoutesMetaMatchesListRoutes(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	par := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	withParent(svc, par)

	addTestRoute(t, svc, "B", map[string]interface{}{"color": "#00ff00"})
	addTestRoute(t, svc, "A", nil)
	parR := mkRouteRec()
	parR.ID, parR.Name, parR.Notes = "par1", "Inherited", "from above"
	if err := routesSave(ctx, par, parR); err != nil {
		t.Fatal(err)
	}

	list, err := svc.ListRoutes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	metas := routesMeta(t, svc)
	if len(metas) != len(list) || len(list) != 3 {
		t.Fatalf("ListRoutes %d vs routes_meta %d, want 3", len(list), len(metas))
	}
	for i := range list {
		if metas[i]["name"] != list[i].Name {
			t.Fatalf("order differs at %d: meta %v, list %q", i, metas[i]["name"], list[i].Name)
		}
		if _, has := metas[i]["waypoints"]; has {
			t.Fatalf("routes_meta should not carry waypoints: %+v", metas[i])
		}
		if id, _ := metas[i]["id"].(string); id == "" {
			t.Fatalf("routes_meta entry without id: %+v", metas[i])
		}
	}
	if metas[0]["color"] != "#00ff00" || metas[0]["scope"] != scopeLocation || metas[0]["source"] != "manual" {
		t.Fatalf("local meta wrong: %+v", metas[0])
	}
	stats, _ := metas[0]["stats"].(map[string]interface{})
	if stats == nil || stats["count"] != float64(2) {
		t.Fatalf("stats wrong: %+v", metas[0]["stats"])
	}
	if metas[2]["id"] != "par1" || metas[2]["scope"] != scopeParent || metas[2]["notes"] != "from above" {
		t.Fatalf("inherited meta wrong: %+v", metas[2])
	}
}

func TestRoutesMetaErrorsWhenLocationUnavailable(t *testing.T) {
	svc := newTestNav(t, nil, errors.New("no location access"))
	if _, err := svc.DoCommand(context.Background(), map[string]interface{}{"routes_meta": true}); err == nil {
		t.Fatal("expected error when the location can't be read")
	}
}

func TestRoutesMetaDedupPrefersLocal(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	par := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	withParent(svc, par)

	// Same id in this location and a parent → the local (location) copy wins.
	shared := mkRouteRec()
	shared.ID, shared.Name = "dup", "FromLocation"
	if err := routesSave(ctx, loc, shared); err != nil {
		t.Fatal(err)
	}
	parDup := mkRouteRec()
	parDup.ID, parDup.Name = "dup", "FromParent"
	if err := routesSave(ctx, par, parDup); err != nil {
		t.Fatal(err)
	}
	metas := routesMeta(t, svc)
	if len(metas) != 1 || metas[0]["scope"] != scopeLocation || metas[0]["name"] != "FromLocation" {
		t.Fatalf("dedup should prefer the local copy: %+v", metas)
	}
}

func TestUpdateRouteColorOnly(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	orig := mkRouteRec()
	orig.ID = "a"
	if err := routesSave(ctx, loc, orig); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRoute(ctx, navigation.UpdateRouteReq{
		Name: "a", Extra: map[string]interface{}{"color": "#abcdef"},
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := routesList(ctx, loc)
	r := recs[0]
	if r.Color != "#abcdef" || r.Name != orig.Name || r.Notes != orig.Notes {
		t.Fatalf("color-only update changed the wrong fields: %+v", r)
	}
	if len(r.Waypoints) != 2 || r.Waypoints[0].Lat != 41.1 {
		t.Fatalf("geometry changed: %+v", r.Waypoints)
	}
	if r.UpdatedAt == orig.UpdatedAt {
		t.Fatal("updatedAt not bumped")
	}
}

func TestUpdateRouteNotesOnly(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	orig := mkRouteRec()
	orig.ID = "a"
	if err := routesSave(ctx, loc, orig); err != nil {
		t.Fatal(err)
	}
	// Addressed by name this time.
	if _, err := svc.UpdateRoute(ctx, navigation.UpdateRouteReq{
		Name: orig.Name, Extra: map[string]interface{}{"notes": "hi"},
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := routesList(ctx, loc)
	if r := recs[0]; r.Notes != "hi" || r.Color != orig.Color || r.Name != orig.Name {
		t.Fatalf("notes-only update changed the wrong fields: %+v", r)
	}
}

func TestUpdateRouteRenameAndReplace(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	orig := mkRouteRec()
	orig.ID = "a"
	if err := routesSave(ctx, loc, orig); err != nil {
		t.Fatal(err)
	}
	name := "Renamed"
	if _, err := svc.UpdateRoute(ctx, navigation.UpdateRouteReq{
		Name:      "a",
		NewName:   &name,
		Waypoints: inlineWaypoints(navPoint{Lat: 1, Lng: 1}, navPoint{Lat: 2, Lng: 2}, navPoint{Lat: 3, Lng: 3}),
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := routesList(ctx, loc)
	r := recs[0]
	if r.ID != "a" || r.Name != "Renamed" || len(r.Waypoints) != 3 || r.Color != orig.Color {
		t.Fatalf("rename+replace wrong: %+v", r)
	}
	if _, err := svc.UpdateRoute(ctx, navigation.UpdateRouteReq{Name: "ghost", NewName: &name}); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestDeleteRouteByID(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	r := mkRouteRec()
	r.ID = "loc1"
	if err := routesSave(ctx, loc, r); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteRoute(ctx, "loc1", nil); err != nil {
		t.Fatal(err)
	}
	if recs, _ := routesList(ctx, loc); len(recs) != 0 {
		t.Error("location store should be empty after delete")
	}
}
