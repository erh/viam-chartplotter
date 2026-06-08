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
| [`wind-publisher`](#wind-publisher) | generic | (optional, one per fleet) publishes ECMWF wind tiles to a CDN |

All models share one MongoDB database (default `osm`). Populate it with
`datasync` + `weathersync` (and `make ingest-osm-*` for the OSM underlay), then
point chartplotter instances at the same Mongo.

---

## chartplotter

`erh:viam-chartplotter:chartplotter` — serves the web UI and renders chart +
weather tiles from MongoDB. Mongo is required; the server reads `osm_*`, `noaa`,
and `weather` collections and renders on demand (no local chart files).

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `mongo_uri` | string | **required** (env `MONGO_URI`) | MongoDB URI holding the ingested chart + weather data |
| `mongo_db` | string | `osm` (env `MONGO_DB`) | database name |
| `port` | int | `8888` | HTTP listen port |
| `draft` | float | `6` | boat draft (ft); drives depth-shading bands (legacy: `safe_depth_ft`) |
| `noaa_cache_dir` | string | OS cache dir | disk cache root for rendered tiles / WMS / weather staging |
| `noaa_cache_max_bytes` | int | `0` (unbounded) | cap on the WMS proxy cache |
| `myboat_icon_path` | string | — | path to a custom boat icon |
| `wind_cdn_base_url` | string | project R2 bucket | base URL for ECMWF wind tiles (see [wind-publisher](#wind-publisher)) |
| `tile_server_base_url` | string | "" (same-origin) | for a split deployment: base URL of a separate tile server the frontend fetches from. Empty = this instance serves its own tiles. |

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

## wind-publisher

`erh:viam-chartplotter:wind-publisher` — optional. Configure on **one** machine
in a fleet: it pulls ECMWF Open Data, crops it into a tile grid, and uploads to a
Cloudflare R2 bucket. Every chartplotter then reads ECMWF wind from that CDN
(`wind_cdn_base_url`) instead of hammering ECMWF. Set `upload_enabled: false`
first to dry-run.

| attribute | type | default | description |
|-----------|------|---------|-------------|
| `models` | []string | **required** | models to publish (currently only `["ecmwf"]`) |
| `upload_enabled` | bool | `false` | actually upload to R2 (false = build only) |
| `r2_account_id` | string | — | Cloudflare account id (required when uploading) |
| `r2_access_key_id` | string | — | R2 token id (or derived from `r2_api_token`) |
| `r2_api_token` | string | — | R2 API token value (SHA-256'd into the SigV4 secret) |
| `r2_secret_access_key` | string | — | precomputed secret (alternative to `r2_api_token`) |
| `r2_bucket` | string | `viam-chartplotter-ecmwf` | target bucket |
| `publish_offset_minutes` | int | `30` | delay after each cycle before publishing |

---

## Building

- `make module` — build the Viam module (`bin/viamchartplottermodule`).
- `make run` — run the server locally (`cmd/run`).
- `make ingest-osm-eastcoast` / `make ingest-osm-all` / `make ingest-noaa` —
  populate MongoDB directly (alternative to the sync models for one-off loads).
- `make updaterdk` — bump the Viam RDK dependency.

Tests: `go test ./...`. The renderer has a golden-image regression test
(`go test ./render -run TestGoldenTiles`, requires a populated Mongo).
