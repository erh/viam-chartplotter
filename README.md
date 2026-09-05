# Viam Chartplotter

A Viam module for marine navigation: a web chartplotter UI that renders NOAA ENC
vector charts + OpenStreetMap + weather, all read from MongoDB. The module ships
several models — one serves the app, the others keep MongoDB populated.

| model | API | role |
|-------|-----|------|
| [`chartplotter`](#chartplotter) | generic | the web UI + tile/weather server (reads charts + weather from Mongo) |
| [`nav`](#navigation) | navigation | persistent waypoint/route navigation service for the UI |
| [`area`](#area) | generic | a region (GeoJSON or center+radius) + color the chartplotter draws as an overlay |
| [`datasync`](#datasync) | generic | keeps the `noaa` collection current (whole NOAA ENC catalog) |
| [`weathersync`](#weathersync) | generic | decodes weather forecasts into the `weather` collection |

All models share one MongoDB database (default `osm`). Populate it with
`datasync` + `weathersync` (and `make ingest-osm-*` for the OSM underlay), then
point chartplotter instances at the same Mongo.

---

## chartplotter

`erh:viam-chartplotter:chartplotter` — serves the web UI and renders chart +
weather tiles from MongoDB. With `mongo_uri` set, the server reads `osm_*`,
`noaa`, and `weather` collections and renders on demand (no local chart files).
**Without `mongo_uri`** it still serves the UI but points the frontend at the
public hosted map+weather server, so tiles and weather work with zero setup.

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `mongo_uri` | string | — (env `MONGO_URI`) | MongoDB URI holding the ingested chart + weather data. Unset → frontend uses the hosted tile/weather server. |
| `mongo_db` | string | `osm` (env `MONGO_DB`) | database name |
| `port` | int | `8888` | HTTP listen port |
| `draft` | float | `6` | boat draft (ft); drives depth-shading bands and is the auto-router's hard floor (legacy: `safe_depth_ft`) |
| `ideal_depth` | float | `0` (= 2x `draft`) | preferred depth (ft) for auto-routing: among safe routes, prefer the one that stays this deep |
| `noaa_cache_dir` | string | OS cache dir | disk cache root for rendered tiles / WMS / weather staging |
| `noaa_cache_max_bytes` | int | `0` (unbounded) | cap on the WMS proxy cache |
| `myboat_icon_path` | string | — | path to a custom boat icon |
| `tile_server_base_url` | string | "" (same-origin; hosted server if `mongo_uri` unset) | base URL of a separate map+weather server the frontend fetches tiles+weather from. Empty = this instance serves its own. |
| `chart_only` | bool | `false` | chart-extended (kiosk) mode: no boat/robot to connect to — the frontend skips the Viam connection and shows only the chart (no boat marker, AIS, nav, camera, or panels). Also auto-enabled when no host is resolvable. |
| `movement_sensor` | string | — | movement sensor for `/api/state` (position/heading/SOG) |
| `depth_sensor` | string | — | depth sensor; adds `depth_ft` to `/api/state` |
| `route_sensor` | string | — | N2K route sensor (e.g. viamboat); feeds `/api/route` |
| `nav_service` | string | — | navigation service; adds waypoints to `/api/route` |
| `cameras` | string[] | — | cameras exposed as `/api/camera/{name}.jpg` |

```json
{
  "name": "chartplotter",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:chartplotter",
  "attributes": {
    "port": 8888,
    "mongo_uri": "mongodb://localhost:27017",
    "draft": 6
  }
}
```

### auto-routing

`GET /noaa-enc/autoroute?startLat=&startLon=&endLat=&endLon=` plans a course
between two points over the charted ENC data and returns a waypoint list.
It rasterises DEPARE depths, land, and obstructions in a corridor around the
rhumb line and A*s across it.

Two depths drive it. **`sd`** (feet, defaults to `draft`) is the hard
constraint — the route never crosses water charted shoaler than it.
**`ideal`** (feet, defaults to `ideal_depth`, else 2x draft) is the soft one:
among the routes that are safe, prefer the one that stays that deep, so the
track sits in the channel rather than shaving its edge.

| param | default | description |
|-------|---------|-------------|
| `sd` | `draft` | safe depth (ft) — hard limit |
| `ideal` | `ideal_depth` | preferred depth (ft) |
| `clearance` | `30` | no-go buffer off land/shoals (m); dropped automatically, with a warning, if no route exists with it |
| `soft_clearance` | `150` | band that merely costs more, which centres the route in a channel (m) |
| `pad` | 40% of the direct distance, min 1 nm | how far off the rhumb line the search may wander (m) |
| `max_cell` | `120` | coarsest grid cell (m) the router will plan on; the leg length limit falls out of this |
| `avoid` | — | comma list of area classes to steer around; currently `restricted` (RESARE). Soft — crossed only when there is no alternative |
| `max_waypoints` | `80` | cap on the returned route |

**How long a leg can be** is bounded by resolution, not distance. The grid
covers the whole corridor in a fixed number of cells, so cell size grows with
the leg — roughly 44 m at 10 nm, 87 m at 20 nm, 218 m at 50 nm, 872 m at
200 nm. Past ~120 m a cell is wider than the channel it is supposed to
represent, so the request is refused with the cell size it would have needed.
That works out to about 28 nm at the default corridor pad; raise `max_cell`
(or narrow `pad`) for a longer open-water passage where that precision isn't
needed.

The response carries `waypoints`, distances, `min_depth_meters` (null when
none of the route was charted), `crossed_unknown`, whether either endpoint had
to be snapped to navigable water, and any `warnings`. Water no DEPARE charts
is passable but penalised, so a survey gap doesn't fail the plan — it flags it.
Soundings are not consulted (the feature store drops their Z coordinate);
DEPARE's DRVAL1, the shoalest depth charted for an area, is what it routes on.

In the web app this is the **Auto route…** form in the Routes panel: pick a
start and destination, plan, review the cautions, then load it into nav or
save it as a route.

### precomputed navigability tiles

Routing reads "how deep is it here, can I go there" over a wide area. Answering
that from polygons meant fetching and rasterising the same water on every
request — measured at **31,052 documents and 58.6 MB** for one 147x80 km
corridor, to produce a grid that is half a megabyte. The `noaa_navgrid`
collection stores the grid instead.

A tile is a 256x256 cell raster over a slippy-map tile: `int16` decimetres of
charted depth plus a flag byte (land, obstruction, dredged, unsurveyed,
restricted), both zlib-compressed. **~2.4 KB per tile** in practice, since a
tile is mostly runs of the same value.

Tiles are **boat-independent on purpose**. They hold charted depth and physical
facts, never a decision that depends on draft — a wreck with a sounding folds
into the depth rather than becoming a blanket obstruction, so a deep-draft boat
and a dinghy read the same tile and reach different, correct conclusions. The
router applies its own safe depth, clearances and penalties to what it reads.

Tiles are built on demand and cached: the first route through a piece of water
pays for it, everything after reads it. Measured end to end:

| route | cold (builds tiles) | warm (reads them) |
|---|---|---|
| 11 nm coastal | 12.6 s | **0.22 s** |
| 258 nm, 6 legs | 17.3 s | **0.11 s** |

Identical routes both ways. To avoid paying the cold cost in front of a user,
prewarm a region: `./chartdiag --mongo … prewarm --start 41.3,-71.6 --end
41.6,-71.1` builds every zoom of the ladder (z9 ≈ 234 m cells through z13 ≈
15 m) for that box — Narragansett Bay is 220 tiles and 0.5 MB.

`NavGridVersion` is part of each document id, so changing what a tile means
rebuilds rather than silently trusting stale data, and a rollback still finds
its own tiles. With no tiles built (or a gap in coverage) routing falls back to
rasterising the polygons, which is correct and slow.

One thing worth knowing if you touch this: tiles are Web-Mercator, so their
rows are **not** evenly spaced in latitude. The router's grid is
equirectangular, so tiles are *sampled* into it per cell rather than stitched
together — stitching and then reading row index as linear latitude misplaced
the chart by 2.6 km (eleven cells) over a three-degree grid.

### optimizing an existing route

`POST /noaa-enc/optimize` re-plans a waypoint list you already have:

```jsonc
{ "waypoints": [{"lat":41.47,"lng":-71.33}, ...],
  "safe_depth_ft": 6, "ideal_depth_ft": 20,
  "keep_waypoints": true }        // the default
```

Every leg is re-planned around land, shoals and obstructions on **one grid over
one chart query** — a ten-waypoint route would otherwise be nine overlapping
fetches of the same water.

`keep_waypoints` (default true) keeps every point you placed and re-plans only
the water between them. A waypoint is usually there for a reason the chart
doesn't record — a stop, a tide gate, something someone saw — so dropping one
isn't a decision to make on your behalf without asking. Set it false to let the
smoother straighten through them and remove the redundant ones.

**Long routes are split into sections.** A 250 nm passage can't be planned on
one grid — its corridor would need cells wider than the channels they
represent. Consecutive legs are greedily grouped into the fewest runs that each
resolve at or below `max_cell`, sharing a chart query wherever they're close
enough to fit one, and the sections are planned concurrently (4 at a time).
A 124 nm, 6-leg route plans in ~14 s; in series it was 40 s. The result reports
`sections`, and `cell_size_meters` is the **coarsest** any section used — the
route is only as well resolved as its worst-resolved part.

A single leg too long to resolve can't be split further, so the error names it:
`leg 2 of 6 is 243.9 nm, which needs 1330 m grid cells (limit 120 m): split it
with an intermediate waypoint…`. An unroutable leg names itself the same way.

The response is otherwise the same shape as `/autoroute`, except
`direct_meters` is the **original route's** length, so you can see what the
optimisation cost or saved.

In the web app it's **Optimize current** beside the Auto route button.

### chart search

`GET /noaa-enc/search?q=<text>[&lat=&lon=][&class=<S-57 acronym>][&limit=<n>]`
finds named features anywhere in the ingested ENC store — lights, wrecks,
canyons, channels, anchorages, harbour facilities. Matching is
case-insensitive substring over the stored `name` (OBJNAM), which is the only
free text the ENC carries; everything else about a feature is codes, so this
is name search, not full-text search over the chart.

Search runs against the **`places` gazetteer**: one collection holding
everything named, drawn from both the ENC and OSM, with a `$text` index over
the names.

It exists because name search over the source collections doesn't scale. An
unanchored regex there is an index scan whose cost tracks the index size rather
than the number of matches — measured at 0.4 s for a common word and 15 s for a
rare one over 274M documents, with the *same query* varying 30x on cache warmth
alone. A text index is inverted, so cost tracks the matches. Measured on the
built region: **0.04-0.2 s**, reliably.

| query | before | now |
|---|---|---|
| `chandler's wharf` | nothing (timed out) | Chandler's Wharf, 0.07 s |
| `marina` near Boothbay | marinas in Boston and Detroit | Boothbay Harbor Marina, 0 nm |

Building it:

```
./datasync --mongo … --build-places --places-bbox=-71.8,40.9,-66.8,44.6
```

Every write is an idempotent upsert keyed on the place's identity, so a build
can be interrupted, resumed, or scoped to a region and extended later — build
the water you sail in first. New England is ~564k places, 103 MB of data and
59 MB of index, and took ~14 minutes. Omit `--places-bbox` for everything.

The identity key rounds position to ~100 m, so the same light charted in the
coastal, approach and harbour cells collapses to one row rather than three.

Two details that matter for matching:

- A quoted phrase in `$text` is **required**, not preferred, so the exact
  phrase is tried as its own query first and the loose terms only if it finds
  nothing. Combining them in one query is phrase-mandatory and returns nothing
  when the name is spelled even slightly differently.
- Ranking puts names carrying **every** word the operator typed above merely
  near ones, comparing on letters only — the data says "Chandler's Wharf", a
  searcher types "chandlers wharf", and treating that as a mismatch buries the
  exact answer under everything sharing the word "wharf".

Results are tagged `source: "chart"` or `"osm"`, because the two answer
different questions: the ENC names lights, wrecks and channels; OSM names
marinas, fuel docks, boatyards and towns. With `lat`/`lon` the results are
ranked nearest-first among equally good name matches.

Where the gazetteer hasn't been built, search falls back to the old regex path
over the source collections — correct, but slow and unreliable for rare names.

In the web app it's the magnifier at the top of the map toolbar: type three or
more characters, and picking a result centres a point feature or frames an
area one.

### display API (LAN thin clients)

When any of `movement_sensor` / `depth_sensor` / `route_sensor` /
`nav_service` / `cameras` are set, the server exposes a small
unauthenticated JSON/JPEG API for display clients on the boat LAN
(e.g. the tvOS app — see TVOS_PLAN.md), and advertises itself over
mDNS/Bonjour as `_viam-chartplotter._tcp` so clients can discover it
with zero configuration. All names are optional dependencies: the
server still comes up if one is missing; that endpoint answers 503.

The attributes are also optional in practice: whenever the web app
connects it reports the resources it discovered for itself to the nav
service (`set_display_resources` DoCommand), and the display API falls
back to those picks for anything the config doesn't name — explicit
config always wins, field by field. Picks persist across module
restarts, so opening the web app once against a machine is enough to
light up the display API for good.

| endpoint | returns |
|----------|---------|
| `GET /api/info` | which endpoints are configured + camera names |
| `GET /api/state` | `{lat, lng, sog_kn, heading_deg, cog_deg, depth_ft, ts}` |
| `GET /api/route` | `{destination_lat/lng, distance_to_waypoint_m, closing_velocity_m_s, waypoints[]}` |
| `GET /api/track` | own-boat track `{points: [{lat, lng, ts}]}`, oldest first — sampled every 10 s in memory (24 h kept), seeded on start with captured position history from the Viam data API when the machine has cloud credentials |
| `GET /api/camera/{name}.jpg` | latest still frame from the named camera |

---

## navigation

`erh:viam-chartplotter:nav` — a `rdk:service:navigation` service that stores the
UI's waypoints/route persistently and reports progress to the next waypoint.

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `movement_sensor` | string | — | name of a movement sensor for position (becomes a dependency) |
| `data_path` | string | — | file path to persist waypoints across restarts |
| `n2k_sender` | string | — | name of a generic component whose DoCommand sends raw NMEA 2000 PGNs (e.g. a `viam-labs:viamboat:sender`; becomes a dependency). Enables `send_waypoints_n2k`. |

```json
{
  "name": "nav",
  "namespace": "rdk",
  "type": "navigation",
  "model": "erh:viam-chartplotter:nav",
  "attributes": { "movement_sensor": "gps", "data_path": "/data/nav.json", "n2k_sender": "n2k-sender" }
}
```

With `n2k_sender` configured, the DoCommand
`{"send_waypoints_n2k": {"route_name"?: string, "database_id"?: int, "route_id"?: int, "dst"?: int}}`
pushes the current waypoint list onto the NMEA 2000 bus as a Route and WP
Service transfer — one PGN 130066 (Route/WP-List Attributes) followed by PGN
130067 (Route – WP Name & Position) messages, chunked to fit fast-packets — so
a chartplotter (e.g. Garmin) on the same backbone can pick the route up. All
payload fields are optional; pass `true` for the defaults (route name
"Chartplotter", broadcast).

---

## area

`erh:viam-chartplotter:area` — a generic component that describes a geographic
region to draw on the chart. Define the region either with **GeoJSON** (a
Geometry, Feature, or FeatureCollection) or with a **center + radius**, and give
it a display **color**. The chartplotter discovers every `area` component on the
machine and draws them as a single **"areas"** overlay, shown by default; the
map's layers panel has an **areas** toggle to hide them.

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `geojson` | object | — | a GeoJSON Geometry, Feature, or FeatureCollection outlining the region |
| `center` | [float, float] | — | `[lat, lng]` of a circular region's center (with `radius_nm`) |
| `radius_nm` | float | — | radius (nautical miles) of the circular region |
| `bearing_min` / `bearing_max` | float | — | optional compass sector (degrees, clockwise from north); draws a pie slice from min→max instead of a full circle |
| `color` | string | `#ff3b30` | CSS color for the outline; drawn with a translucent fill |
| `start_date` | string | — | inclusive start month-day `MM-DD` (no year/time); hidden before this day |
| `end_date` | string | — | inclusive end month-day `MM-DD` (no year/time); hidden after this day |

Supply `geojson`, or `center` + `radius_nm`, or both (at least one is required).
Discovery works by probing each generic component with a `{"get_area": true}`
DoCommand, so no naming convention is needed.

`bearing_min` / `bearing_max` (set both, or neither) cut the circle down to a
compass wedge — degrees clockwise from true north, drawn from `bearing_min`
around to `bearing_max`. `bearing_min > bearing_max` wraps through north (e.g.
`315`→`45` is the northern sector). Handy for "150 nm south of here" without
hand-writing a polygon.

`start_date` / `end_date` optionally limit *when* the area is drawn as recurring
`MM-DD` month-days (no year), so the window repeats every year. Either can be set
alone for an open-ended range, and a range may wrap across the year end (e.g.
start `12-01`, end `02-01`). The chartplotter compares them against the local
date and only shows the area on days inside the (inclusive) window, so seasonal
regions appear and disappear on their own.

```json
{
  "name": "restricted-zone",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:area",
  "attributes": {
    "center": [40.69, -74.04],
    "radius_nm": 0.5,
    "color": "#ff3b30"
  }
}
```

A compass wedge — 150 nm south (SE→S→SW) of a point, shown July 10–19:

```json
{
  "name": "montauk-canyons",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:area",
  "attributes": {
    "center": [40.694, -72.048],
    "radius_nm": 150,
    "bearing_min": 135,
    "bearing_max": 225,
    "color": "#3b82f6",
    "start_date": "07-10",
    "end_date": "07-19"
  }
}
```

An explicit GeoJSON polygon, shown each summer:

```json
{
  "name": "survey-box",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:area",
  "attributes": {
    "color": "#3b82f6",
    "start_date": "06-01",
    "end_date": "09-01",
    "geojson": {
      "type": "Polygon",
      "coordinates": [[[-74.05,40.68],[-74.02,40.68],[-74.02,40.70],[-74.05,40.70],[-74.05,40.68]]]
    }
  }
}
```

---

## datasync

`erh:viam-chartplotter:datasync` — periodically refreshes the NOAA ENC catalog
and ingests **every published cell worldwide** into the `noaa` collection. Runs
once on start, then every `interval_hours`; cells already at the current edition
are skipped, so re-runs are cheap. Run on one machine in a fleet.

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `mongo_uri` | string | **required** | MongoDB URI to populate |
| `mongo_db` | string | `osm` | database name |
| `enc_dir` | string | OS cache dir | ENC download/staging directory |
| `min_scale` / `max_scale` | int | `0` (no bound) | restrict ingest by chart compilation scale |
| `parallel` | int | `4` | concurrent cell downloads |
| `interval_hours` | int | `24` | sync interval |

```json
{
  "name": "datasync",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:datasync",
  "attributes": { "mongo_uri": "mongodb://localhost:27017", "interval_hours": 24 }
}
```

> OSM data is **not** synced here — load it with `make ingest-osm-*` (it changes
> slowly and is a large one-off batch).

---

## weathersync

`erh:viam-chartplotter:weathersync` — decodes weather forecasts (GFS wind/wave,
isobars, …) from GRIB and writes the served JSON/GeoJSON to the `weather`
collection every `interval_hours`. Chartplotter/tile servers then serve weather
from Mongo instead of each re-fetching GRIB.

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `mongo_uri` | string | **required** | MongoDB URI to populate |
| `mongo_db` | string | `osm` | database name |
| `cache_dir` | string | OS cache dir | GRIB decode/staging directory (auto-cleaned) |
| `models` | []string | all enabled | restrict to specific model names, e.g. `["gfs","ecmwf"]` |
| `max_fh` | int | per-model max | cap the forecast hour synced |
| `interval_hours` | int | `6` | sync interval |

```json
{
  "name": "weathersync",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:weathersync",
  "attributes": { "mongo_uri": "mongodb://localhost:27017", "interval_hours": 6 }
}
```

---

## Building

- `make module` — build the Viam module (`bin/viamchartplottermodule`).
- `make run` — run the server locally (`cmd/run`), serving the bundled `dist`.
- `make dev` — the local test loop: the Go server on :8888 **and** the Vite dev
  server on :5173 with hot reload, in one foreground process (Ctrl+C stops
  both). Vite proxies `/noaa-enc`, `/noaa-weather`, `/app-config` and friends to
  :8888, so the dev app behaves like the bundled one. Point it at chart data
  with `make dev MONGO_URI=mongodb://localhost:27017` — without it the frontend
  falls back to the hosted tile server and `/noaa-enc/autoroute` returns 503.
  The Routes panel needs a live navigation service, so open
  `http://localhost:5173/?host=<machine>.viam.cloud&api-key=<key>&authEntity=<key-id>`
  to exercise it; with no params the app comes up chart-only.
- `make ingest-osm-eastcoast` / `make ingest-osm-all` / `make ingest-noaa` —
  populate MongoDB directly (alternative to the sync models for one-off loads).
- `make chartdiag` — build the query/prewarm tool. When a chart query is slow or
  times out, this says why: which indexes exist, the exact filter the server
  runs, its `explain()` plan (index or COLLSCAN, keys and docs examined), and a
  timed live run with the payload size.
  ```
  ./chartdiag --mongo mongodb://host:27017 route --start 41.47,-71.33 --end 41.55,-71.39
  ./chartdiag --mongo mongodb://host:27017 search --q brenton
  ./chartdiag --mongo mongodb://host:27017 osm --q marina
  ./chartdiag --mongo mongodb://host:27017 prewarm --start 41.3,-71.6 --end 41.6,-71.1
  ```
- `make ensure-indexes` — create/refresh the `noaa` collection's indexes
  without re-ingesting (`datasync --indexes-only`). **Run this after pulling a
  change that adds an index** — `name_search` (chart search) and `class_geo`
  (auto-router) are both new. Seconds, against data that's already there; a
  missing index is the usual cause of a `context deadline exceeded` from
  Mongo.
- `make updaterdk` — bump the Viam RDK dependency.

### Overview-tile speed (optional backfills)

Low-zoom tiles cover a huge area, so their `$geoIntersects` walks a large
2dsphere index over full-resolution coastlines/depth areas. Curated low-zoom
collections with pre-simplified geometry make them fast; build them once (re-run
after a sync to refresh):

- `make backfill-noaa-lowzoom` — builds `noaa_lowzoom` (the z7..z10 overview
  band, valid-simplified geometry). Cuts the overview NOAA query ~3-4×. The
  renderer falls back to the full `noaa` collection when it's absent, so this is
  purely a speed-up.
- `make backfill-geomlow` / `make backfill-lowzoom` — the OSM equivalents for the
  `osm_lowzoom` band.

Tests: `go test ./...`. The renderer has a golden-image regression test
(`go test ./render -run TestGoldenTiles`, requires a populated Mongo).
