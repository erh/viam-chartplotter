# Viam Chartplotter

A Viam module for marine navigation: a web chartplotter UI that renders NOAA ENC
vector charts + OpenStreetMap + weather, all read from MongoDB. The module ships
several models — one serves the app, the others keep MongoDB populated.

| model | API | role |
|-------|-----|------|
| [`chartplotter`](#chartplotter) | generic | the web UI + tile/weather server (reads charts + weather from Mongo) |
| [`nav`](#navigation) | navigation | persistent waypoint/route navigation service for the UI |
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

```json
{
  "name": "nav",
  "namespace": "rdk",
  "type": "navigation",
  "model": "erh:viam-chartplotter:nav",
  "attributes": { "movement_sensor": "gps", "data_path": "/data/nav.json" }
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
