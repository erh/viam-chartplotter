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
| `draft` | float | `6` | boat draft (ft); drives depth-shading bands (legacy: `safe_depth_ft`) |
| `noaa_cache_dir` | string | OS cache dir | disk cache root for rendered tiles / WMS / weather staging |
| `noaa_cache_max_bytes` | int | `0` (unbounded) | cap on the WMS proxy cache |
| `myboat_icon_path` | string | — | path to a custom boat icon |
| `tile_server_base_url` | string | "" (same-origin; hosted server if `mongo_uri` unset) | base URL of a separate map+weather server the frontend fetches tiles+weather from. Empty = this instance serves its own. |
| `chart_only` | bool | `false` | chart-extended (kiosk) mode: no boat/robot to connect to — the frontend skips the Viam connection and shows only the chart (no boat marker, AIS, nav, camera, or panels). Also auto-enabled when no host is resolvable. |

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
- `make run` — run the server locally (`cmd/run`).
- `make ingest-osm-eastcoast` / `make ingest-osm-all` / `make ingest-noaa` —
  populate MongoDB directly (alternative to the sync models for one-off loads).
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
