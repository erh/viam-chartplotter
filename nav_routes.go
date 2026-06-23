package vc

import (
	"context"
	"encoding/json"
	"os"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"

	"go.viam.com/rdk/app"
	rutils "go.viam.com/rdk/utils"
)

// Saved routes live in the Viam location metadata under a single key, shared
// across every machine/user in the location. We own only this key and preserve
// every other key on each read-modify-write. The browser reaches this through
// the nav service's routes_* DoCommand verbs (see nav.go) so it never needs its
// own cloud credentials — the nav service authenticates with the machine's own
// VIAM_API_KEY/VIAM_LOCATION_ID env vars instead.
const (
	routesKey           = "chartplotter_routes"
	routesSchemaVersion = 1
	// Hard ceiling on the serialized routes blob. The whole location metadata
	// blob must stay well under the gRPC message cap; routes are only one key.
	routesMaxBytes = 400 * 1024

	// scope tags where a route lives, set on list responses (not persisted).
	// location = this machine's own location; parent = inherited from an
	// ancestor location (read-only here); robot = local fallback on this machine
	// when the location can't be written.
	scopeLocation = "location"
	scopeParent   = "parent"
	scopeRobot    = "robot"
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
	// Scope is set on list responses ("location"/"robot"); it is cleared before
	// a route is persisted, so storage stays authoritative about location.
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

func applyRouteFields(r *routeRec, fields map[string]interface{}) {
	if v, ok := fields["name"].(string); ok {
		r.Name = v
	}
	if v, ok := fields["notes"].(string); ok {
		r.Notes = v
	}
	if v, ok := fields["color"].(string); ok {
		r.Color = v
	}
	if v, ok := fields["updatedAt"].(string); ok {
		r.UpdatedAt = v
	}
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

func routesRename(ctx context.Context, store metadataStore, id string, fields map[string]interface{}) error {
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
	found := false
	for i := range blob.Routes {
		if blob.Routes[i].ID != id {
			continue
		}
		found = true
		applyRouteFields(&blob.Routes[i], fields)
		break
	}
	if !found {
		return errors.Errorf("route %s not found", id)
	}
	return commitRoutes(ctx, store, meta, blob)
}

// --- DoCommand handlers -----------------------------------------------------

func routesToInterface(routes []routeRec) ([]interface{}, error) {
	b, err := json.Marshal(routes)
	if err != nil {
		return nil, err
	}
	var out []interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []interface{}{}
	}
	return out, nil
}

// doRoutesList aggregates routes from everywhere they can live: the robot-local
// fallback, this machine's own location, and every ancestor (parent) location —
// each tagged with its scope. Routes are deduped by id keeping the most-local
// copy (robot > location > parent), then ordered shared → inherited → local.
// Location is best-effort: if it can't be reached the robot routes are still
// returned, with location_available false and a location_error.
func (s *navService) doRoutesList(ctx context.Context) (map[string]interface{}, error) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()

	byID := map[string]routeRec{}
	add := func(routes []routeRec, scope string) {
		for _, r := range routes {
			if _, exists := byID[r.ID]; exists {
				continue // a more-local copy already won
			}
			r.Scope = scope
			r.Stats = computeRouteStats(r.Waypoints)
			byID[r.ID] = r
		}
	}

	// Most-local first so it wins the dedup.
	add(s.robotRoutes.list(), scopeRobot)

	locationAvailable := false
	locationErr := ""
	if store, err := s.routesStore(ctx); err == nil {
		if routes, lerr := routesList(ctx, store); lerr == nil {
			locationAvailable = true
			add(routes, scopeLocation)
			// Parent locations only make sense once we can read the location.
			if parents, perr := s.parentStores(ctx); perr != nil {
				s.logger.Debugw("could not walk parent locations for routes", "error", perr)
			} else {
				for _, ps := range parents {
					if routes, err := routesList(ctx, ps); err == nil {
						add(routes, scopeParent)
					}
				}
			}
		} else {
			locationErr = lerr.Error()
		}
	} else {
		locationErr = err.Error()
	}

	// Order for display: shared (this location), then inherited, then local.
	var loc, par, rob []routeRec
	for _, r := range byID {
		switch r.Scope {
		case scopeLocation:
			loc = append(loc, r)
		case scopeParent:
			par = append(par, r)
		default:
			rob = append(rob, r)
		}
	}
	all := append(append(loc, par...), rob...)

	iface, err := routesToInterface(all)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"routes": iface, "location_available": locationAvailable}
	if locationErr != "" {
		out["location_error"] = locationErr
	}
	return out, nil
}

func parseRoutePayload(raw interface{}) (routeRec, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return routeRec{}, errors.New("routes_save payload must be an object")
	}
	routeRaw, ok := m["route"]
	if !ok {
		return routeRec{}, errors.New("routes_save.route is required")
	}
	b, err := json.Marshal(routeRaw)
	if err != nil {
		return routeRec{}, err
	}
	var r routeRec
	if err := json.Unmarshal(b, &r); err != nil {
		return routeRec{}, errors.Wrap(err, "parsing routes_save.route")
	}
	return r, nil
}

// doRoutesSave saves (or replaces) a route. If a route with this id already
// lives on the robot it's updated there — so editing/replacing a robot route
// stays local and doesn't silently promote (use routes_promote for that). New
// routes try the location first; if that can't be written (no cloud access /
// permission) they fall back to the robot, reporting scope:"robot" plus the
// location_error so the UI can offer "Promote" later.
func (s *navService) doRoutesSave(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	r, err := parseRoutePayload(raw)
	if err != nil {
		return nil, err
	}
	// Validate up front so an invalid route never silently lands on the robot.
	if err := validateRoute(r); err != nil {
		return nil, err
	}

	s.routesMu.Lock()
	defer s.routesMu.Unlock()

	// An existing robot route is updated in place (no accidental promotion, and
	// no duplicate id appearing in both stores).
	if _, ok := s.robotRoutes.get(r.ID); ok {
		r.Stats = computeRouteStats(r.Waypoints)
		if err := s.robotRoutes.upsert(r); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "id": r.ID, "scope": scopeRobot}, nil
	}

	var locErr error
	if store, serr := s.routesStore(ctx); serr == nil {
		locErr = locationUpsert(ctx, store, r)
		if locErr == nil {
			return map[string]interface{}{"ok": true, "id": r.ID, "scope": scopeLocation}, nil
		}
	} else {
		locErr = serr
	}

	// Couldn't write the location — keep it on the robot.
	r.Stats = computeRouteStats(r.Waypoints)
	if err := s.robotRoutes.upsert(r); err != nil {
		return nil, err
	}
	out := map[string]interface{}{"ok": true, "id": r.ID, "scope": scopeRobot}
	if locErr != nil {
		out["location_error"] = locErr.Error()
	}
	return out, nil
}

// doRoutesPromote moves a robot-local route up to the shared location store.
// Requires location write access; on success the robot copy is removed.
func (s *navService) doRoutesPromote(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("routes_promote payload must be an object")
	}
	id, _ := m["id"].(string)
	if id == "" {
		return nil, errors.New("routes_promote.id is required")
	}
	s.routesMu.Lock()
	defer s.routesMu.Unlock()

	r, ok := s.robotRoutes.get(id)
	if !ok {
		return nil, errors.Errorf("robot route %s not found", id)
	}
	store, err := s.routesStore(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "cannot promote: no location access")
	}
	if err := locationUpsert(ctx, store, r); err != nil {
		return nil, errors.Wrap(err, "promoting route to location")
	}
	if _, err := s.robotRoutes.delete(id); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "id": id, "scope": scopeLocation}, nil
}

// doRoutesDelete removes a route from wherever it lives: the robot store if it's
// there, otherwise the location (which needs access).
func (s *navService) doRoutesDelete(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("routes_delete payload must be an object")
	}
	id, _ := m["id"].(string)
	s.routesMu.Lock()
	defer s.routesMu.Unlock()

	robotDeleted, err := s.robotRoutes.delete(id)
	if err != nil {
		return nil, err
	}
	if robotDeleted {
		return map[string]interface{}{"ok": true, "scope": scopeRobot}, nil
	}
	store, err := s.routesStore(ctx)
	if err != nil {
		return nil, err
	}
	if err := routesDelete(ctx, store, id); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "scope": scopeLocation}, nil
}

// doRoutesRename updates mutable fields on a route in whichever store holds it.
func (s *navService) doRoutesRename(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("routes_rename payload must be an object")
	}
	id, _ := m["id"].(string)
	s.routesMu.Lock()
	defer s.routesMu.Unlock()

	renamed, err := s.robotRoutes.rename(id, m)
	if err != nil {
		return nil, err
	}
	if renamed {
		return map[string]interface{}{"ok": true, "scope": scopeRobot}, nil
	}
	store, err := s.routesStore(ctx)
	if err != nil {
		return nil, err
	}
	if err := routesRename(ctx, store, id, m); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "scope": scopeLocation}, nil
}
