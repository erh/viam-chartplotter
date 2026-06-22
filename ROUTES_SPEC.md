# Spec: Routes as a first-class feature

## Goal

Make **routes** first-class objects in the chartplotter:

1. **Store routes** durably in the Viam **location metadata** (`GetLocationMetadata` /
   `UpdateLocationMetadata`), shared across every machine and user in the location.
2. **Load a saved route into the nav service** — replace the active waypoint list with the
   route's waypoints in one atomic operation.
3. **Save a recorded track as a route** — take the boat's logged position history between two
   timestamps, reduce it to a manageable set of waypoints by a distance granularity, and save it.

### Design decisions (locked)

| Decision | Choice |
|---|---|
| Where the logic lives | **Hybrid** — frontend (browser) owns metadata read/write + track capture; backend nav service gets **one new bulk `set_waypoints` DoCommand** so a route loads atomically. |
| Track → route reduction | **Decimate, then Douglas–Peucker** (distance decimation first, DP cleanup second). |
| Saved-route model | **Immutable snapshots.** Geometry never changes after save. Load = copy into nav. "Editing" geometry = save a new route + delete the old. Rename / recolor / delete are allowed (they don't touch geometry). |

Non-goals (this pass): route optimization/auto-routing, hazard avoidance, multi-leg ETA stored
in metadata (ETA stays a live UI derivation), cross-location route sharing.

---

## Data model

### Location-metadata blob

Metadata is **one `Struct` (JSON object) per location**, round-tripped as
`Record<string, JsonValue>`. We own a single top-level key, `chartplotter_routes`, and **must
preserve every other key** on read-modify-write (other tools may use the same blob).

```jsonc
// value stored under metadata["chartplotter_routes"]
{
  "schemaVersion": 1,
  "routes": [
    {
      "id": "rte_<base36-time>_<rand4>",   // client-generated, stable
      "name": "Block Island Run",
      "notes": "",                          // optional free text
      "color": "#ff8800",                   // optional, for map display; default assigned
      "source": "manual" | "track",         // how it was created
      "createdAt": "2026-06-19T14:02:11Z",  // ISO-8601 UTC
      "updatedAt": "2026-06-19T14:02:11Z",  // == createdAt unless renamed/recolored
      "waypoints": [
        { "lat": 41.1631, "lng": -71.5784 },
        { "lat": 41.2042, "lng": -71.5511 }
      ],
      "stats": { "distanceNm": 23.4, "count": 12 }  // derived cache, NOT authoritative
    }
  ]
}
```

Notes:
- `id` is generated client-side (no server allocator). Format keeps it sortable-ish and unique
  without `Math.random` concerns — e.g. `rte_` + `Date.now().toString(36)` + 4 hex chars.
- `stats` is a convenience for the list UI; always recomputed from `waypoints` on load, never
  trusted for correctness.
- Immutable means `waypoints` is frozen post-save. `name`/`notes`/`color` may change; bump
  `updatedAt` when they do.

### Active waypoints (unchanged shape)

The nav service still owns the single active list. Frontend mirror stays
`globalData.navWaypoints: { id: string; lat: number; lng: number }[]` (`App.svelte:111`).
A loaded route's points become ordinary waypoints — no back-reference to the route id is stored
(immutable model means the active list is free to diverge immediately).

### Size budget

`google.protobuf.Struct` over gRPC ⇒ keep the whole blob well under ~256 KB. A 100-point route
≈ 5 KB JSON, so ~40 routes of that size is the soft ceiling. UI warns when the serialized
`chartplotter_routes` value exceeds **200 KB** and blocks save above **400 KB**.

---

## Backend changes (Go)

Minimal: one bulk replace verb so loading a route is atomic (no N round-trips, no arrival-poller
churn / map flicker between inserts).

### `nav_store.go` — new store method

```go
// ReplaceWaypoints atomically swaps the entire active waypoint list for the given
// ordered points. All previous waypoints (including visited ones) are discarded and
// fresh ObjectIDs + order indices are assigned. Persists once.
func (s *diskNavStore) ReplaceWaypoints(ctx context.Context, points []*geo.Point) (int, error)
```

- Takes the lock once, rebuilds `s.waypoints` with new `primitive.NewObjectID()` per point and
  sequential `Order`, clears any visited entries, calls `save()` once, returns the count.
- Mirrors the existing `InsertWaypoint`/`MoveWaypoint` style (`nav_store.go:214,264`).

### `nav.go` — new DoCommand verb `set_waypoints`

Add alongside `move_waypoint` / `insert_waypoint` / `arrival_status` (`nav.go:408`):

```jsonc
// request
{ "set_waypoints": { "waypoints": [ { "lat": 41.16, "lng": -71.57 }, ... ] } }
// response
{ "count": 12 }
```

- Validate payload is an object with a `waypoints` array; each element needs numeric `lat`/`lng`.
- Empty array is legal and means "clear the route" (replaces the current clear-all loop in
  `App.svelte:1972`).
- Calls `store.ReplaceWaypoints`. Document the verb in the DoCommand doc-comment block.

### Backend tests

`nav_store_test.go`: `ReplaceWaypoints` assigns fresh IDs, preserves order, clears visited,
empty input clears, persists across reload. A `DoCommand({set_waypoints})` round-trip test.

No new dependencies, no cloud creds in the module — the backend never touches location metadata.

---

## Frontend changes (TS / Svelte)

### New module: `src/lib/routeStore.ts`

Pure-ish wrapper over the app client. Holds no Svelte state; callers own state.

```ts
export interface Route {
  id: string; name: string; notes?: string; color?: string;
  source: "manual" | "track";
  createdAt: string; updatedAt: string;
  waypoints: { lat: number; lng: number }[];
  stats?: { distanceNm: number; count: number };
}

// All take the already-created appClient (globalCloudClient.appClient) + locationId.
listRoutes(appClient, locationId): Promise<Route[]>
saveRoute(appClient, locationId, route: Route): Promise<void>      // upsert by id
deleteRoute(appClient, locationId, id: string): Promise<void>
renameRoute(appClient, locationId, id, name, notes?, color?): Promise<void>
```

Implementation rules:
- Every mutation is **read-modify-write**: `getLocationMetadata(locationId)` → clone → mutate the
  `chartplotter_routes.routes` array → `updateLocationMetadata(locationId, merged)`. **Preserve
  all other top-level keys.** Last-write-wins (acceptable: route edits are rare, single-operator
  boats). Document the race; no locking.
- Tolerate a missing/empty `chartplotter_routes` key (first run) → treat as `{schemaVersion:1, routes:[]}`.
- If `schemaVersion` is newer than known, surface a "saved by a newer version" warning and go
  read-only rather than clobbering.

### New module: `src/lib/simplify.ts` (pure, unit-tested)

```ts
// Reduce a time-ordered polyline to route waypoints.
//   1. distance decimation: keep a point only if >= granularityMeters from last kept point
//   2. Douglas–Peucker with toleranceMeters (default granularityMeters / 2)
// Always keeps first & last. Distances use haversine; DP perpendicular distance uses a local
// equirectangular projection scaled by cos(lat) so tolerances are in meters.
simplifyTrack(
  points: { lat: number; lng: number; t?: number }[],
  opts: { granularityMeters: number; toleranceMeters?: number; maxPoints?: number },
): { lat: number; lng: number }[]
```

- `maxPoints` (e.g. 500) is a hard safety cap; if DP still exceeds it, raise tolerance and re-run
  (or evenly subsample) and report that it was capped.
- Pure functions → cheap to unit-test (straight line collapses to 2 points; a square keeps its 4
  corners; sub-granularity jitter is dropped).

### Track source for "save track as route"

Reuse the existing position-history pipeline (`globalData.posHistory`, populated by the
`positionHistoryMQL` path in `App.svelte`). Two concerns:

1. **Time window.** Current history is capped to the last 7 days. For an arbitrary `[t0, t1]`,
   parametrize the MQL query's time bounds (a `fetchTrackWindow(t0, t1)` helper) rather than
   reusing the 7-day default, so older windows can be pulled on demand. If `[t0,t1]` is fully
   inside what's already in `posHistory`, just filter in memory — no fetch.
2. **Empty/short window** → disable Save, show "no track in this window".

### nav loading / clearing rewired to `set_waypoints`

- **Load route** → `NavigationClient.doCommand({ set_waypoints: { waypoints } })`, then refresh
  `globalData.navWaypoints` from the poll (or optimistically set it). Replaces the active list.
  Confirm first if the active list is non-empty ("Replace current N waypoints?").
- **Clear all** (`App.svelte:1972`) → `set_waypoints: { waypoints: [] }` instead of the
  remove-loop.
- `addWayPoint` / `insert_waypoint` / `move_waypoint` / `removeWayPoint` are unchanged.

### UI

A **Routes** section (new collapsible panel, or a tab in the existing controls):

- **List** of saved routes: name, leg count, total distance (NM), source badge (manual/track),
  date. Row actions: **Load**, **Show on map** (preview), **Rename**, **Delete**.
- **Save current as route** — snapshots `globalData.navWaypoints` → `saveRoute(source:"manual")`,
  prompts for a name.
- **Save track as route** flow:
  1. Pick **start/end time** — two time inputs; sensible defaults (e.g. last 24 h, or a brush over
     the existing track timeline if one exists).
  2. Pick **granularity** (distance input; default **200 m**) and optional tolerance.
  3. **Preview**: run `simplifyTrack`, render the candidate polyline on the map + show point count
     and total distance; live-update as granularity changes. Warn if capped at `maxPoints`.
  4. Name + **Save** (`source:"track"`).
- **Disabled state** when there is no cloud connection / no `locationId` (see edge cases): show
  the panel greyed with "Routes need a cloud (Viam app) connection."

### Map rendering of a previewed/selected route

Render the selected or preview route as a **distinct polyline** (its `color`, dimmed, lower
z-index than the active nav route), reusing the existing line/marker layer pattern in
`marineMap.svelte` (`navRouteLineFeatures` ~1035–1070). It's display-only — not draggable, not the
active nav route — until **Load** copies it into nav.

### Wiring points (confirmed symbols)

- App client: `globalCloudClient.appClient` (`App.svelte:1077`). Cloud client created at
  `App.svelte:1069`.
- Location id: `globalClientCloudMetaData.locationId` (`App.svelte:1392`, populated by
  `getCloudMetadata()` at `App.svelte:1672`).
- Active waypoints mirror: `globalData.navWaypoints` (`App.svelte:111`); nav verbs via
  `new VIAM.NavigationClient(globalClient, globalConfig.navServiceName)` (`App.svelte:400,1905`).
- Track history: `globalData.posHistory` + the `positionHistoryMQL` path in `App.svelte`.

---

## Edge cases & failure modes

- **No cloud / no locationId** (e.g. `viam module reload-local` against a local-only machine):
  `getLocationMetadata` is unavailable. Detect (no `globalCloudClient` or empty `locationId`) and
  put the Routes UI in the disabled state above. **Load into nav still works** for an in-session
  route, but persistence is off. Must be tested explicitly — local dev often has no location.
- **Concurrent edits** from two clients: last-write-wins on the whole blob; always re-GET
  immediately before each write to shrink the window. Document; don't over-engineer.
- **Preserve foreign keys** in the metadata blob — never send back only `chartplotter_routes`.
- **Size**: warn >200 KB, block >400 KB (serialized `chartplotter_routes`).
- **Schema drift**: unknown newer `schemaVersion` → read-only + warning.
- **Bad waypoints** (NaN, out of range) rejected client-side before save and by the backend
  `set_waypoints` validator.
- **Empty track window** → Save disabled.

---

## Build / verification

- Go: `go build ./...`, `go test ./...` (new `nav_store_test.go` cases).
- Frontend: `npx svelte-check --tsconfig ./tsconfig.json` (0 errors) + `NODE_ENV=development npm run build`.
- Unit tests for `simplifyTrack` (no browser needed).
- Manual (`viam module reload-local`, user-run):
  1. Save current waypoints as a route → reload page → route still listed (cloud round-trip).
  2. Load route → active waypoints replaced atomically, no flicker.
  3. Save track as route → adjust granularity, watch preview point count change → save → reload.
  4. Delete / rename.
  5. Local-only machine (no location) → panel disabled with the right message.

---

## Build order (each step compiles & is testable)

1. **Backend** `ReplaceWaypoints` + `set_waypoints` DoCommand + tests. (Independent; ship first.)
2. **`simplify.ts`** + unit tests. (Pure, independent.)
3. **`routeStore.ts`** — list/save/delete/rename over `appClient`, preserve-keys RMW.
4. **Routes panel UI** — list + Save-current + Load (via `set_waypoints`) + Delete/Rename;
   rewire Clear-all to `set_waypoints:[]`.
5. **Save-track-as-route** flow — time window + granularity + live `simplifyTrack` preview.
6. **Map preview rendering** of the selected/preview route.

---

## Open questions for build time (defaults chosen, easy to revisit)

- Granularity units in the UI: meters vs. boat-length vs. NM. Default **meters (200 m)**.
- Default route colors: cycle a small palette, or one fixed color. Default **cycle palette**.
- Where the Routes UI lives: new panel vs. tab in existing controls. Default **new collapsible panel**.
- Time-window picker UX: numeric datetime inputs vs. brushing the existing track on the map.
  Default **datetime inputs**, upgrade to brush later.
