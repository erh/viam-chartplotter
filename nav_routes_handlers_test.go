package vc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.viam.com/rdk/logging"
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

func savePayload(r routeRec) map[string]interface{} {
	b, _ := json.Marshal(r)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return map[string]interface{}{"route": m}
}

func TestRoutesSaveUsesLocation(t *testing.T) {
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
	locRoutes, _ := routesList(ctx, loc)
	if len(locRoutes) != 1 {
		t.Fatalf("location should hold the route, got %d", len(locRoutes))
	}
}

func TestRoutesSaveValidationRejected(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, newFakeMetaStore(nil), nil)
	bad := mkRouteRec()
	bad.Name = "" // invalid
	if _, err := svc.doRoutesSave(ctx, savePayload(bad)); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRoutesSaveErrorsWhenLocationUnavailable(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, nil, errors.New("no location access"))
	if _, err := svc.doRoutesSave(ctx, savePayload(mkRouteRec())); err == nil {
		t.Fatal("expected error when the location can't be written")
	}
}

func TestRoutesListLocation(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)

	locR := mkRouteRec()
	locR.ID = "loc1"
	if _, err := svc.doRoutesSave(ctx, savePayload(locR)); err != nil {
		t.Fatal(err)
	}

	out, err := svc.doRoutesList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	routes := out["routes"].([]interface{})
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	m := routes[0].(map[string]interface{})
	if m["id"] != "loc1" || m["scope"] != scopeLocation {
		t.Fatalf("route wrong: %+v", m)
	}
}

func TestRoutesListErrorsWhenLocationUnavailable(t *testing.T) {
	ctx := context.Background()
	svc := newTestNav(t, nil, errors.New("no location access"))
	if _, err := svc.doRoutesList(ctx); err == nil {
		t.Fatal("expected error when the location can't be read")
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

	out, err := svc.doRoutesList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	routes := out["routes"].([]interface{})
	if len(routes) != 2 {
		t.Fatalf("want 2 routes (location+parent), got %d", len(routes))
	}
	scopes := map[string]string{}
	for _, r := range routes {
		m := r.(map[string]interface{})
		scopes[m["id"].(string)] = m["scope"].(string)
	}
	if scopes["loc1"] != scopeLocation || scopes["par1"] != scopeParent {
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

func TestRoutesDeleteLocation(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)

	locR := mkRouteRec()
	locR.ID = "loc1"
	if _, err := svc.doRoutesSave(ctx, savePayload(locR)); err != nil {
		t.Fatal(err)
	}
	out, err := svc.doRoutesDelete(ctx, map[string]interface{}{"id": "loc1"})
	if err != nil {
		t.Fatal(err)
	}
	if out["scope"] != scopeLocation {
		t.Errorf("delete scope = %v, want location", out["scope"])
	}
	if locRoutes, _ := routesList(ctx, loc); len(locRoutes) != 0 {
		t.Error("location store should be empty after delete")
	}
}
