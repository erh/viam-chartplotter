package vc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"go.viam.com/rdk/logging"
)

func newTestRobotStore(t *testing.T) *diskRoutesStore {
	t.Helper()
	rs, err := newDiskRoutesStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// newTestNav builds a navService wired to a robot-local store plus an injected
// location store (or a location error to simulate "no access").
func newTestNav(t *testing.T, locStore metadataStore, locErr error) *navService {
	t.Helper()
	svc := &navService{robotRoutes: newTestRobotStore(t), logger: logging.NewTestLogger(t)}
	switch {
	case locErr != nil:
		svc.routesStoreFn = func(_ context.Context) (metadataStore, error) { return nil, locErr }
	case locStore != nil:
		svc.routesStoreFn = func(_ context.Context) (metadataStore, error) { return locStore, nil }
	}
	return svc
}

func savePayload(r routeRec) map[string]interface{} {
	b, _ := json.Marshal(r)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return map[string]interface{}{"route": m}
}

func TestDiskRoutesStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	rs, err := newDiskRoutesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := mkRouteRec()
	if err := rs.upsert(r); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces by id.
	r.Name = "Renamed"
	if err := rs.upsert(r); err != nil {
		t.Fatal(err)
	}
	if got := rs.list(); len(got) != 1 || got[0].Name != "Renamed" {
		t.Fatalf("list = %+v, want 1 named Renamed", got)
	}
	// Persists across reload.
	rs2, err := newDiskRoutesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := rs2.list(); len(got) != 1 || got[0].ID != r.ID {
		t.Fatalf("reloaded list = %+v", got)
	}
	// Delete.
	ok, err := rs2.delete(r.ID)
	if err != nil || !ok {
		t.Fatalf("delete ok=%v err=%v", ok, err)
	}
	if got := rs2.list(); len(got) != 0 {
		t.Fatalf("after delete len = %d", len(got))
	}
}

func TestRoutesSaveFallsBackToRobot(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, nil, errors.New("no location access"))

	out, err := svc.doRoutesSave(ctx, savePayload(mkRouteRec()))
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeRobot {
		t.Fatalf("scope = %v, want robot", out["scope"])
	}
	if out["location_error"] == nil {
		t.Error("expected location_error to be reported")
	}
	if len(svc.robotRoutes.list()) != 1 {
		t.Fatalf("robot store should hold the fallback route")
	}
}

func TestRoutesSaveUsesLocationWhenAvailable(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)

	out, err := svc.doRoutesSave(ctx, savePayload(mkRouteRec()))
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeLocation {
		t.Fatalf("scope = %v, want location", out["scope"])
	}
	if len(svc.robotRoutes.list()) != 0 {
		t.Error("robot store should be empty when location write succeeds")
	}
	locRoutes, _ := routesList(ctx, loc)
	if len(locRoutes) != 1 {
		t.Fatalf("location should hold the route, got %d", len(locRoutes))
	}
}

func TestRoutesSaveValidationDoesNotFallBack(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, nil, errors.New("no location access"))
	bad := mkRouteRec()
	bad.Name = "" // invalid
	if _, err := svc.doRoutesSave(ctx, savePayload(bad)); err == nil {
		t.Fatal("expected validation error, not a robot fallback")
	}
	if len(svc.robotRoutes.list()) != 0 {
		t.Error("invalid route must not be written to the robot store")
	}
}

func TestRoutesListMergesScopes(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)

	locR := mkRouteRec()
	locR.ID = "loc1"
	if _, err := svc.doRoutesSave(ctx, savePayload(locR)); err != nil {
		t.Fatal(err)
	}
	robR := mkRouteRec()
	robR.ID = "rob1"
	if err := svc.robotRoutes.upsert(robR); err != nil {
		t.Fatal(err)
	}

	out, err := svc.doRoutesList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["location_available"] != true {
		t.Errorf("location_available = %v, want true", out["location_available"])
	}
	routes := out["routes"].([]interface{})
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	scopes := map[string]string{}
	for _, r := range routes {
		m := r.(map[string]interface{})
		scopes[m["id"].(string)] = m["scope"].(string)
	}
	if scopes["loc1"] != scopeLocation || scopes["rob1"] != scopeRobot {
		t.Fatalf("scopes wrong: %+v", scopes)
	}
}

func TestRoutesListLocationUnavailable(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, nil, errors.New("no location access"))
	if err := svc.robotRoutes.upsert(mkRouteRec()); err != nil {
		t.Fatal(err)
	}
	out, err := svc.doRoutesList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["location_available"] != false {
		t.Errorf("location_available = %v, want false", out["location_available"])
	}
	if out["location_error"] == nil {
		t.Error("expected location_error")
	}
	if len(out["routes"].([]interface{})) != 1 {
		t.Error("robot routes should still be listed when location is down")
	}
}

func TestRoutesPromote(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)

	r := mkRouteRec()
	if err := svc.robotRoutes.upsert(r); err != nil {
		t.Fatal(err)
	}
	out, err := svc.doRoutesPromote(ctx, map[string]interface{}{"id": r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeLocation {
		t.Fatalf("scope = %v, want location", out["scope"])
	}
	if len(svc.robotRoutes.list()) != 0 {
		t.Error("robot copy should be removed after promote")
	}
	locRoutes, _ := routesList(ctx, loc)
	if len(locRoutes) != 1 || locRoutes[0].ID != r.ID {
		t.Fatalf("location should hold the promoted route: %+v", locRoutes)
	}
}

func TestRoutesPromoteNeedsLocationAccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, nil, errors.New("no location access"))
	r := mkRouteRec()
	if err := svc.robotRoutes.upsert(r); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.doRoutesPromote(ctx, map[string]interface{}{"id": r.ID}); err == nil {
		t.Fatal("promote should fail without location access")
	}
	if len(svc.robotRoutes.list()) != 1 {
		t.Error("robot copy must remain when promote fails")
	}
}

func TestRoutesSaveUpdatesExistingRobotRouteInPlace(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil) // location is available...
	svc := newTestNav(t, loc, nil)

	r := mkRouteRec()
	if err := svc.robotRoutes.upsert(r); err != nil { // ...but the route already lives on the robot
		t.Fatal(err)
	}
	r.Name = "Updated"
	out, err := svc.doRoutesSave(ctx, savePayload(r))
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeRobot {
		t.Fatalf("scope = %v, want robot (save must not promote)", out["scope"])
	}
	if locRoutes, _ := routesList(ctx, loc); len(locRoutes) != 0 {
		t.Fatalf("save must not duplicate the route into the location, got %d", len(locRoutes))
	}
	got := svc.robotRoutes.list()
	if len(got) != 1 || got[0].Name != "Updated" {
		t.Fatalf("robot route not updated in place: %+v", got)
	}
}

func TestRoutesListIncludesParents(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	par := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	svc.parentStoresFn = func(_ context.Context) ([]metadataStore, error) {
		return []metadataStore{par}, nil
	}

	locR := mkRouteRec()
	locR.ID = "loc1"
	if _, err := svc.doRoutesSave(ctx, savePayload(locR)); err != nil {
		t.Fatal(err)
	}
	parR := mkRouteRec()
	parR.ID = "par1"
	if err := routesSave(ctx, par, parR); err != nil {
		t.Fatal(err)
	}
	rob := mkRouteRec()
	rob.ID = "rob1"
	if err := svc.robotRoutes.upsert(rob); err != nil {
		t.Fatal(err)
	}

	out, err := svc.doRoutesList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	routes := out["routes"].([]interface{})
	if len(routes) != 3 {
		t.Fatalf("want 3 routes (location+parent+robot), got %d", len(routes))
	}
	scopes := map[string]string{}
	for _, r := range routes {
		m := r.(map[string]interface{})
		scopes[m["id"].(string)] = m["scope"].(string)
	}
	if scopes["loc1"] != scopeLocation || scopes["par1"] != scopeParent || scopes["rob1"] != scopeRobot {
		t.Fatalf("scopes wrong: %+v", scopes)
	}
}

func TestRoutesListDedupPrefersLocal(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	par := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	svc.parentStoresFn = func(_ context.Context) ([]metadataStore, error) {
		return []metadataStore{par}, nil
	}

	// Same id in this location and a parent → the local (location) copy wins.
	shared := mkRouteRec()
	shared.ID = "dup"
	shared.Name = "FromLocation"
	if _, err := svc.doRoutesSave(ctx, savePayload(shared)); err != nil {
		t.Fatal(err)
	}
	parDup := mkRouteRec()
	parDup.ID = "dup"
	parDup.Name = "FromParent"
	if err := routesSave(ctx, par, parDup); err != nil {
		t.Fatal(err)
	}

	out, err := svc.doRoutesList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	routes := out["routes"].([]interface{})
	if len(routes) != 1 {
		t.Fatalf("want 1 deduped route, got %d", len(routes))
	}
	m := routes[0].(map[string]interface{})
	if m["scope"] != scopeLocation || m["name"] != "FromLocation" {
		t.Fatalf("dedup should prefer the local copy: %+v", m)
	}
}

func TestRoutesDeleteRobotThenLocation(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)

	// A robot route and a location route with distinct ids.
	rob := mkRouteRec()
	rob.ID = "rob1"
	if err := svc.robotRoutes.upsert(rob); err != nil {
		t.Fatal(err)
	}
	locR := mkRouteRec()
	locR.ID = "loc1"
	if _, err := svc.doRoutesSave(ctx, savePayload(locR)); err != nil {
		t.Fatal(err)
	}

	out, err := svc.doRoutesDelete(ctx, map[string]interface{}{"id": "rob1"})
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeRobot {
		t.Errorf("delete rob1 scope = %v, want robot", out["scope"])
	}
	out, err = svc.doRoutesDelete(ctx, map[string]interface{}{"id": "loc1"})
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeLocation {
		t.Errorf("delete loc1 scope = %v, want location", out["scope"])
	}
	if len(svc.robotRoutes.list()) != 0 {
		t.Error("robot store should be empty")
	}
	locRoutes, _ := routesList(ctx, loc)
	if len(locRoutes) != 0 {
		t.Error("location store should be empty")
	}
}
