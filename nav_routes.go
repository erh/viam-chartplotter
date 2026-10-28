package vc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"

	"go.viam.com/rdk/app"
	"go.viam.com/rdk/services/navigation"
	rutils "go.viam.com/rdk/utils"
)

// Saved routes live in the Viam location metadata under a single key, shared
// across every machine/user in the location. We own only this key and preserve
// every other key on each read-modify-write. Clients reach them through the
// navigation API's Route RPCs (AddRoute/ListRoutes/UpdateRoute/DeleteRoute),
// with the fields the API has no slot for (notes, color, source) carried in
// extra, plus the routes_meta DoCommand to read those fields back. The nav
// service authenticates with the machine's own VIAM_API_KEY/VIAM_LOCATION_ID
// env vars, so clients never need their own cloud credentials.
const (
	routesKey           = "chartplotter_routes"
	routesSchemaVersion = 1
	// Hard ceiling on the serialized routes blob. The whole location metadata
	// blob must stay well under the gRPC message cap; routes are only one key.
	routesMaxBytes = 400 * 1024

	// scope tags where a route lives, set on list responses (not persisted).
	// location = this machine's own location; parent = inherited from an
	// ancestor location (read-only here).
	scopeLocation = "location"
	scopeParent   = "parent"
)

type routeWaypoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type routeStatsRec struct {
	DistanceNm float64 `json:"distanceNm"`
	Count      int     `json:"count"`
}

type routeRec struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Notes     string          `json:"notes,omitempty"`
	Color     string          `json:"color,omitempty"`
	Source    string          `json:"source"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
	Waypoints []routeWaypoint `json:"waypoints"`
	Stats     *routeStatsRec  `json:"stats,omitempty"`
	// Scope is set on list responses ("location"/"parent"); it is cleared
	// before a route is persisted, so storage stays authoritative.
	Scope string `json:"scope,omitempty"`
}

type routesBlob struct {
	SchemaVersion int        `json:"schemaVersion"`
	Routes        []routeRec `json:"routes"`
}

// metadataStore is the minimal location-metadata surface the routes logic
// needs. Backed by the real Viam app client in production; an in-memory fake in
// tests.
type metadataStore interface {
	Get(ctx context.Context) (map[string]interface{}, error)
	Update(ctx context.Context, meta map[string]interface{}) error
}

type appMetadataStore struct {
	ac         *app.AppClient
	locationID string
}

func (a *appMetadataStore) Get(ctx context.Context) (map[string]interface{}, error) {
	return a.ac.GetLocationMetadata(ctx, a.locationID)
}

func (a *appMetadataStore) Update(ctx context.Context, meta map[string]interface{}) error {
	return a.ac.UpdateLocationMetadata(ctx, a.locationID, meta)
}

// ensureAppClient lazily builds (and caches) the Viam app client + this
// machine's location id. Caller must hold s.routesMu.
func (s *navService) ensureAppClient(ctx context.Context) (*app.AppClient, string, error) {
	if s.appClient == nil {
		locationID := os.Getenv(rutils.LocationIDEnvVar)
		if locationID == "" {
			return nil, "", errors.Errorf(
				"%s is not set — routes need a cloud-connected machine", rutils.LocationIDEnvVar)
		}
		vc, err := app.CreateViamClientFromEnvVars(ctx, nil, s.logger)
		if err != nil {
			return nil, "", errors.Wrap(err,
				"creating Viam app client for routes (machine needs an API key)")
		}
		s.viamClient = vc
		s.appClient = vc.AppClient()
		s.locationID = locationID
	}
	return s.appClient, s.locationID, nil
}

// routesStore returns the metadata store for this machine's own location.
// Caller must hold s.routesMu.
func (s *navService) routesStore(ctx context.Context) (metadataStore, error) {
	if s.routesStoreFn != nil {
		return s.routesStoreFn(ctx)
	}
	ac, locationID, err := s.ensureAppClient(ctx)
	if err != nil {
		return nil, err
	}
	return &appMetadataStore{ac: ac, locationID: locationID}, nil
}

// parentStores returns metadata stores for every ancestor location, nearest
// first, by walking ParentLocationID up from this machine's location. Routes
// in ancestor locations are inherited (read-only) here. Best-effort: a lookup
// error returns the ancestors found so far. Caller must hold s.routesMu.
func (s *navService) parentStores(ctx context.Context) ([]metadataStore, error) {
	if s.parentStoresFn != nil {
		return s.parentStoresFn(ctx)
	}
	ac, locationID, err := s.ensureAppClient(ctx)
	if err != nil {
		return nil, err
	}
	var stores []metadataStore
	seen := map[string]bool{locationID: true}
	cur := locationID
	// Bounded walk; location nesting is shallow and the guard also stops cycles.
	for i := 0; i < 16; i++ {
		loc, err := ac.GetLocation(ctx, cur)
		if err != nil {
			return stores, err
		}
		parent := loc.ParentLocationID
		if parent == "" || seen[parent] {
			break
		}
		seen[parent] = true
		stores = append(stores, &appMetadataStore{ac: ac, locationID: parent})
		cur = parent
	}
	return stores, nil
}

// --- pure read-modify-write logic (operates on any metadataStore) -----------

func parseRoutesBlob(meta map[string]interface{}) (routesBlob, error) {
	raw, ok := meta[routesKey]
	if !ok || raw == nil {
		return routesBlob{SchemaVersion: routesSchemaVersion}, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return routesBlob{}, errors.Wrap(err, "re-marshaling existing routes metadata")
	}
	var blob routesBlob
	if err := json.Unmarshal(b, &blob); err != nil {
		return routesBlob{}, errors.Wrap(err, "parsing existing routes metadata")
	}
	if blob.SchemaVersion == 0 {
		blob.SchemaVersion = routesSchemaVersion
	}
	return blob, nil
}

func computeRouteStats(wps []routeWaypoint) *routeStatsRec {
	total := 0.0
	for i := 1; i < len(wps); i++ {
		a := geo.NewPoint(wps[i-1].Lat, wps[i-1].Lng)
		b := geo.NewPoint(wps[i].Lat, wps[i].Lng)
		total += a.GreatCircleDistance(b) // kilometers
	}
	return &routeStatsRec{DistanceNm: total / 1.852, Count: len(wps)}
}

// commitRoutes writes the blob back, preserving all foreign keys, after the
// schema-version and size guards.
func commitRoutes(ctx context.Context, store metadataStore, meta map[string]interface{}, blob routesBlob) error {
	if blob.SchemaVersion > routesSchemaVersion {
		return errors.Errorf(
			"routes were saved by a newer chartplotter (schema v%d); refusing to overwrite", blob.SchemaVersion)
	}
	blob.SchemaVersion = routesSchemaVersion
	data, err := json.Marshal(blob)
	if err != nil {
		return err
	}
	if len(data) > routesMaxBytes {
		return errors.Errorf(
			"saved routes would be %d KB, over the %d KB limit; delete some routes first",
			len(data)/1024, routesMaxBytes/1024)
	}
	meta[routesKey] = blob
	return store.Update(ctx, meta)
}

func routesList(ctx context.Context, store metadataStore) ([]routeRec, error) {
	meta, err := store.Get(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := parseRoutesBlob(meta)
	if err != nil {
		return nil, err
	}
	// Recompute stats from the authoritative geometry; never trust stored ones.
	for i := range blob.Routes {
		blob.Routes[i].Stats = computeRouteStats(blob.Routes[i].Waypoints)
	}
	return blob.Routes, nil
}

func validateRoute(r routeRec) error {
	if r.ID == "" {
		return errors.New("route id is required")
	}
	if r.Name == "" {
		return errors.New("route name is required")
	}
	if len(r.Waypoints) < 2 {
		return errors.New("route needs at least 2 waypoints")
	}
	for i, wp := range r.Waypoints {
		if wp.Lat < -90 || wp.Lat > 90 || wp.Lng < -180 || wp.Lng > 180 {
			return errors.Errorf("waypoint %d out of range (lat=%v lng=%v)", i, wp.Lat, wp.Lng)
		}
	}
	return nil
}

// locationUpsert writes a (already-validated) route into the location metadata.
func locationUpsert(ctx context.Context, store metadataStore, r routeRec) error {
	r.Scope = ""
	r.Stats = computeRouteStats(r.Waypoints)
	meta, err := store.Get(ctx)
	if err != nil {
		return err
	}
	blob, err := parseRoutesBlob(meta)
	if err != nil {
		return err
	}
	replaced := false
	for i := range blob.Routes {
		if blob.Routes[i].ID == r.ID {
			blob.Routes[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		blob.Routes = append(blob.Routes, r)
	}
	return commitRoutes(ctx, store, meta, blob)
}

func routesSave(ctx context.Context, store metadataStore, r routeRec) error {
	if err := validateRoute(r); err != nil {
		return err
	}
	return locationUpsert(ctx, store, r)
}

func routesDelete(ctx context.Context, store metadataStore, id string) error {
	if id == "" {
		return errors.New("route id is required")
	}
	meta, err := store.Get(ctx)
	if err != nil {
		return err
	}
	blob, err := parseRoutesBlob(meta)
	if err != nil {
		return err
	}
	out := blob.Routes[:0]
	for _, r := range blob.Routes {
		if r.ID != id {
			out = append(out, r)
		}
	}
	blob.Routes = out
	return commitRoutes(ctx, store, meta, blob)
}

// --- DoCommand handlers -----------------------------------------------------

// allRoutes aggregates routes from this machine's own location and every
// ancestor (parent) location, each tagged with its scope. Routes are deduped by
// id keeping the most-local copy (location > parent), then ordered shared →
// inherited. Caller must hold s.routesMu.
func (s *navService) allRoutes(ctx context.Context) ([]routeRec, error) {
	byID := map[string]routeRec{}
	var order []string
	add := func(routes []routeRec, scope string) {
		for _, r := range routes {
			if _, exists := byID[r.ID]; exists {
				continue // a more-local copy already won
			}
			r.Scope = scope
			r.Stats = computeRouteStats(r.Waypoints)
			byID[r.ID] = r
			order = append(order, r.ID)
		}
	}

	store, err := s.routesStore(ctx)
	if err != nil {
		return nil, err
	}
	routes, err := routesList(ctx, store)
	if err != nil {
		return nil, err
	}
	add(routes, scopeLocation)
	// Parent locations are inherited (read-only) here.
	if parents, perr := s.parentStores(ctx); perr != nil {
		s.logger.Debugw("could not walk parent locations for routes", "error", perr)
	} else {
		for _, ps := range parents {
			if routes, err := routesList(ctx, ps); err == nil {
				add(routes, scopeParent)
			}
		}
	}

	// Insertion order is already shared (this location) then inherited.
	all := make([]routeRec, 0, len(order))
	for _, id := range order {
		all = append(all, byID[id])
	}
	return all, nil
}

// routeMeta is a saved route's record minus its waypoints: what routes_meta
// returns for the fields the API's Route message can't carry.
type routeMeta struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Notes     string         `json:"notes,omitempty"`
	Color     string         `json:"color,omitempty"`
	Source    string         `json:"source"`
	CreatedAt string         `json:"createdAt"`
	UpdatedAt string         `json:"updatedAt"`
	Stats     *routeStatsRec `json:"stats,omitempty"`
	Scope     string         `json:"scope,omitempty"`
}

// doRoutesMeta handles {"routes_meta": true}: the metadata of every saved
// route, in the same order as ListRoutes, so a client can pair the two lists
// up (by index, falling back to name).
func (s *navService) doRoutesMeta(ctx context.Context) (map[string]interface{}, error) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	all, err := s.allRoutes(ctx)
	if err != nil {
		return nil, err
	}
	metas := make([]routeMeta, len(all))
	for i, r := range all {
		metas[i] = routeMeta{
			ID: r.ID, Name: r.Name, Notes: r.Notes, Color: r.Color, Source: r.Source,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Stats: r.Stats, Scope: r.Scope,
		}
	}
	b, err := json.Marshal(metas)
	if err != nil {
		return nil, err
	}
	out := []interface{}{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return map[string]interface{}{"routes": out}, nil
}

// --- navigation API Route RPCs ---------------------------------------------------
//
// The API identifies a route by name and carries only its waypoints; the
// rest of each record (id, notes, color, source, dates, scope) goes in extra on
// writes and comes back through the routes_meta DoCommand. Names aren't guaranteed unique in that store, so every lookup also
// accepts a route's id, and a name resolves to the most-local match (this
// location before inherited ones). Inherited routes are read-only.

// matchRoute returns the first route whose id, then whose name, is key.
func matchRoute(routes []routeRec, key string) (routeRec, bool) {
	for _, r := range routes {
		if r.ID == key {
			return r, true
		}
	}
	for _, r := range routes {
		if r.Name == key {
			return r, true
		}
	}
	return routeRec{}, false
}

func apiRoute(r routeRec) navigation.Route {
	pts := make([]navPoint, len(r.Waypoints))
	for i, w := range r.Waypoints {
		pts[i] = navPoint{Lat: w.Lat, Lng: w.Lng}
	}
	return navigation.Route{Name: r.Name, Waypoints: positionWaypoints(pts)}
}

// findRoute looks a saved route up by id or name across this location and
// its ancestors.
func (s *navService) findRoute(ctx context.Context, key string) (routeRec, error) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	all, err := s.allRoutes(ctx)
	if err != nil {
		return routeRec{}, err
	}
	r, ok := matchRoute(all, key)
	if !ok {
		return routeRec{}, errors.Errorf("route %q not found", key)
	}
	return r, nil
}

func (s *navService) routeWaypoints(wps []navigation.Waypoint) ([]routeWaypoint, error) {
	pts, err := s.resolveWaypoints(wps)
	if err != nil {
		return nil, err
	}
	out := make([]routeWaypoint, len(pts))
	for i, p := range pts {
		out[i] = routeWaypoint{Lat: p.Lat, Lng: p.Lng}
	}
	return out, nil
}

// newRouteID matches the web app's id format (routeStore.ts newRouteId):
// "rte_" + base36 epoch millis + 4 hex chars.
func newRouteID(now time.Time) string {
	return fmt.Sprintf("rte_%s_%04x", strconv.FormatInt(now.UnixMilli(), 36), rand.Intn(0x10000))
}

func nowISO(now time.Time) string {
	return now.UTC().Format("2006-01-02T15:04:05.000Z")
}

// AddRoute saves a new route in this location. Extra may carry the fields the
// API has no slot for: "notes", "color" and "source" ("manual" by default).
func (s *navService) AddRoute(ctx context.Context, req navigation.AddRouteReq) (navigation.Route, error) {
	wps, err := s.routeWaypoints(req.Waypoints)
	if err != nil {
		return navigation.Route{}, err
	}
	now := time.Now()
	r := routeRec{
		ID:        newRouteID(now),
		Name:      req.Name,
		Source:    "manual",
		CreatedAt: nowISO(now),
		UpdatedAt: nowISO(now),
		Waypoints: wps,
	}
	str := func(key string) string { v, _ := req.Extra[key].(string); return v }
	r.Notes, r.Color = str("notes"), str("color")
	if src := str("source"); src != "" {
		r.Source = src
	}
	if err := validateRoute(r); err != nil {
		return navigation.Route{}, err
	}

	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	store, err := s.routesStore(ctx)
	if err != nil {
		return navigation.Route{}, err
	}
	existing, err := routesList(ctx, store)
	if err != nil {
		return navigation.Route{}, err
	}
	for _, e := range existing {
		if e.Name == r.Name {
			return navigation.Route{}, errors.Errorf("route %q already exists", r.Name)
		}
	}
	if err := locationUpsert(ctx, store, r); err != nil {
		return navigation.Route{}, err
	}
	return apiRoute(r), nil
}

func (s *navService) GetRoute(ctx context.Context, name string, extra map[string]interface{}) (navigation.Route, error) {
	r, err := s.findRoute(ctx, name)
	if err != nil {
		return navigation.Route{}, err
	}
	return apiRoute(r), nil
}

func (s *navService) ListRoutes(ctx context.Context, extra map[string]interface{}) ([]navigation.Route, error) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	all, err := s.allRoutes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]navigation.Route, len(all))
	for i, r := range all {
		out[i] = apiRoute(r)
	}
	return out, nil
}

// locationRoute finds a route in this location's own store for a mutation;
// a route that only exists in an ancestor location is read-only.
func (s *navService) locationRoute(ctx context.Context, store metadataStore, key string) (routeRec, error) {
	own, err := routesList(ctx, store)
	if err != nil {
		return routeRec{}, err
	}
	if r, ok := matchRoute(own, key); ok {
		return r, nil
	}
	if all, err := s.allRoutes(ctx); err == nil {
		if _, ok := matchRoute(all, key); ok {
			return routeRec{}, errors.Errorf("route %q is inherited from a parent location and is read-only here", key)
		}
	}
	return routeRec{}, errors.Errorf("route %q not found", key)
}

// UpdateRoute renames a route and/or replaces its waypoints. Extra may carry
// "notes" and "color", so a notes- or color-only update needs neither.
func (s *navService) UpdateRoute(ctx context.Context, req navigation.UpdateRouteReq) (navigation.Route, error) {
	var wps []routeWaypoint
	if req.Waypoints != nil {
		var err error
		if wps, err = s.routeWaypoints(req.Waypoints); err != nil {
			return navigation.Route{}, err
		}
	}
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	store, err := s.routesStore(ctx)
	if err != nil {
		return navigation.Route{}, err
	}
	r, err := s.locationRoute(ctx, store, req.Name)
	if err != nil {
		return navigation.Route{}, err
	}
	if req.NewName != nil {
		r.Name = *req.NewName
	}
	if req.Waypoints != nil {
		r.Waypoints = wps
	}
	// The API's Route has no slot for these; they ride in extra.
	if v, ok := req.Extra["notes"].(string); ok {
		r.Notes = v
	}
	if v, ok := req.Extra["color"].(string); ok {
		r.Color = v
	}
	r.UpdatedAt = nowISO(time.Now())
	if err := validateRoute(r); err != nil {
		return navigation.Route{}, err
	}
	if err := locationUpsert(ctx, store, r); err != nil {
		return navigation.Route{}, err
	}
	return apiRoute(r), nil
}

func (s *navService) DeleteRoute(ctx context.Context, name string, extra map[string]interface{}) error {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	store, err := s.routesStore(ctx)
	if err != nil {
		return err
	}
	r, err := s.locationRoute(ctx, store, name)
	if err != nil {
		return err
	}
	return routesDelete(ctx, store, r.ID)
}
