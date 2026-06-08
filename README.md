## Viam Chartplotter

The `erh:viam-chartplotter:chartplotter` model is an `rdk:component:generic`
that hosts the chartplotter web UI and the NOAA ENC / WMS rendering server.
It serves the static frontend (`dist/`), proxies and caches NOAA WMS tiles
under `/noaa-wms/`, and renders ENC vector tiles under `/noaa-enc/`.

### Configuration attributes

| Name                   | Type   | Required | Default | Description                                                                                                                                                                                                                  |
| ---------------------- | ------ | -------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `port`                 | int    | no       | `8888`  | TCP port the embedded HTTP server listens on.                                                                                                                                                                                |
| `noaa_cache_dir`       | string | no       | OS user-cache dir (`<cache>/viam-chartplotter/`) | Directory under which the NOAA WMS proxy cache (`noaa-wms/`), the ENC store (`noaa-enc/`), and the OSM raster cache (`osm/`) live. Created on first run.                       |
| `noaa_cache_max_bytes` | int    | no       | `0` (unlimited) | Soft cap for the WMS proxy cache (bytes). When exceeded the oldest tiles get evicted. `0` disables eviction.                                                                                                          |
| `draft`                | number | no       | `6`     | Boat's draft in feet. Drives depth shading at chart-detail zoom: DEPMS covers 3.3 ft → draft, DEPMD covers draft → 2×draft, DEPDW (safe water, white) is ≥ 2×draft. Per-request override via `?sd=N` on tile URLs.            |
| `safe_depth_ft`        | number | no       | —       | Legacy alias for `draft`. Used when `draft` is not set.                                                                                                                                                                      |
| `myboat_icon_path`     | string | no       | bundled icon | Absolute path to a PNG used as the boat marker on the chart. Falls back to the bundled icon when unset.                                                                                                                  |
| `mongo_uri`            | string | no       | env `MONGO_URI` | MongoDB connection URI holding the ingested map data (`osm_*` + `noaa` collections). Enables the vector chart + OSM underlay. Env `MONGO_URI` is the fallback.                                                          |
| `mongo_db`             | string | no       | `osm` (env `MONGO_DB`) | Database name within Mongo that holds the collections.                                                                                                                                                         |
| `tile_server_base_url` | string | no       | see below | Base URL of a separate map+weather server the frontend should fetch tiles/weather from (e.g. `http://host:8989`). Empty = same-origin (this server serves its own tiles). When unset but Mongo is configured, defaults to `http://localhost:8989`. |

### Sample config

```json
{
  "name": "chartplotter",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:chartplotter",
  "attributes": {
    "port": 8888,
    "draft": 6,
    "noaa_cache_dir": "/var/lib/viam-chartplotter"
  }
}
```

### Map + weather server only

For a fleet, run one instance as a shared **map + weather server** that renders
tiles and serves weather straight from MongoDB, and point the chartplotter app
instances at it. (Today this is a chartplotter instance with Mongo configured;
the rendering reads `osm_*` + `noaa` from Mongo, so it needs no local ENC/OSM
files. A dedicated headless tile server — no UI — is planned.)

Backend (serves `/noaa-enc/tile/...` and `/noaa-weather/...` from Mongo on
:8989):

```json
{
  "name": "mapserver",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:chartplotter",
  "attributes": {
    "port": 8989,
    "mongo_uri": "mongodb://db-host:27017",
    "mongo_db": "osm"
  }
}
```

App instances fetch tiles/weather from it instead of rendering locally:

```json
{
  "name": "chartplotter",
  "namespace": "rdk",
  "type": "generic",
  "model": "erh:viam-chartplotter:chartplotter",
  "attributes": {
    "port": 8888,
    "tile_server_base_url": "http://map-host:8989"
  }
}
```

**Resolving the map-server address** (highest precedence first):

1. `tile_server_base_url` config attribute (explicit override)
2. if Mongo is configured (so a backend is expected) → `http://localhost:8989`
   (`DefaultTileServerURL`)
3. otherwise empty → same-origin (this instance serves its own tiles)

So an app configured with Mongo automatically points at a local `:8989` map
server, and any deployment overrides that with the `tile_server_base_url`
attribute. Populate the database with the `make ingest-*` targets (see
[Building](#building)).

### Keeping MongoDB populated

The chartplotter / tile server render everything from MongoDB: `osm_overview/
coastal/detail/skip` (OSM), `noaa` (parsed S-57 ENC features), and `weather`
(decoded forecasts) — all in one database. Three ways to fill it:

- **One-off / batch** — the `mapsync` CLI and `make` targets:
  `make ingest-noaa`, `make ingest-osm-eastcoast`, `make ingest-osm-all`,
  `make ingest-all` (override `MONGO=…`). Best for the big, infrequent OSM
  state extracts.
- **Scheduled (Viam)** — two cron-style models you add to one machine in the
  fleet, writing to the shared DB:
  - <a name="datasync"></a>`erh:viam-chartplotter:datasync` — periodically
    refreshes the NOAA ENC catalog and syncs **every published cell worldwide**
    into `noaa` (NOAA publishes new editions weekly; cells already current are
    skipped). Config: `mongo_uri`, `mongo_db`, `enc_dir`, `min_scale`/`max_scale`
    (optional scale bounds), `parallel`, `interval_hours` (default 24).
  - <a name="weathersync"></a>`erh:viam-chartplotter:weathersync` — decodes GRIB (GFS wind/wave,
    isobars) and writes the served JSON to `weather` so tile servers serve
    weather from Mongo instead of each re-fetching GRIB. Config: `mongo_uri`,
    `mongo_db`, `models` (optional filter), `max_fh`, `interval_hours`
    (default 6).
- **Scheduled (standalone)** — the same loops as plain daemons for non-Viam
  hosts: `datasync` and `weathersync` (build with `make datasync` /
  `make weathersync`).

A headless map+weather server (no UI) is `make tileserver` →
`MONGO_URI=… ./tileserver --port 8989`.

### Depth shading bands

Both bands key off the polygon's midpoint depth (`(DRVAL1 + DRVAL2) / 2`).
With the default `draft = 6 ft`:

**z ≥ 12** (chart-detail, four-band):

| Midpoint depth      | Band  | Colour              |
| ------------------- | ----- | ------------------- |
| `< 0` (drying)      | DEPIT | tan                 |
| `0 – 3.3 ft` (< 1 m)| DEPVS | saturated blue      |
| `3.3 ft – draft`    | DEPMS | light blue          |
| `draft – 2×draft`   | DEPMD | very light blue     |
| `≥ 2×draft`         | DEPDW | white (safe water)  |

**z ≤ 11** (coarse, two-band):

| Midpoint depth | Band  | Colour              |
| -------------- | ----- | ------------------- |
| `< 0`          | DEPIT | tan                 |
| `0 – 2×draft`  | DEPVS | saturated blue      |
| `≥ 2×draft`    | DEPDW | white (safe water)  |

### Optional wind overlay

The chartplotter can show animated wind particles using
[sakitam-fdd/wind-layer](https://github.com/sakitam-fdd/wind-layer). It's wired
up as a togglable layer (off by default) and appears in the layers panel as
`wind` once the package is installed and wind data is published.

Wind data is served by the bundled NOAA weather cache at
`/noaa-weather/gfs/latest.json` (see below). The cache fetches the latest
GFS 0.25° UGRD/VGRD at 10 m above ground from NOMADS, parses the GRIB2
inline, and writes the JSON shape `ol-wind` consumes. No external
converter required — `grib2json`, ecCodes, Java, etc. are *not* needed.

To enable:

1. `npm install` (the `ol-wind` package is already listed in `package.json`).
2. Rebuild the frontend (`npm run build`) and reload — toggle `wind` on
   from the layers panel.

If `ol-wind` is missing or NOMADS is unreachable the chartplotter logs a
warning and leaves the layer out — nothing else breaks.

### NOAA weather endpoints

| Path                              | Purpose                                                                                                              |
| --------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `/noaa-weather/gfs/latest.json`   | Latest GFS 0.25° UGRD + VGRD at 10 m, two-record JSON shaped for `ol-wind`. Disk-cached under `<root>/noaa-weather/`, soft TTL 90 min with stale-while-revalidate. Fetches the most recent published GFS cycle from NOMADS (walks back in 6 h steps until one returns 200). |
| `/noaa-weather/stats`             | JSON cache stats: hits, refreshes, errors, current file size and mtime.                                              |

### ECMWF wind: one machine populates the cache for the whole fleet

ECMWF Open Data publishes a complete forecast every 6 h, and a chartplotter
fleet of any size would crush their free tier (and trip the rate limiter)
if every machine pulled directly. So one machine in the fleet runs the
`erh:viam-chartplotter:wind-publisher` model, which:

1. Wakes up on a 15-minute heartbeat
2. Walks the latest fully-published ECMWF cycle (newest first, falls back
   one cycle at a time if the freshest one is still publishing)
3. Decodes 10u + 10v at each forecast hour (0…144 in 3 h steps = 49 fhs)
4. Crops each fh into a fixed 6 × 6 grid of overlapping tiles
5. Uploads each gzipped tile blob + a manifest + a `latest.json` pointer
   to Cloudflare R2

Every other chartplotter in the fleet reads tiles from R2 (zero ECMWF
traffic). Default bucket: `viam-chartplotter-ecmwf`, default public URL:
`https://pub-6ae2d2a870f74799a963dbc892ea400b.r2.dev`.

#### Setting up the publisher machine

You need exactly one machine running this component. Pick whichever one
has reliable network and modest CPU to spare (the build phase is ~5–15 min
per cycle but only fires after a new ECMWF cycle publishes, ~4×/day).

1. **Create an R2 bucket** (one-time, project-wide). Cloudflare dashboard
   → R2 → Create bucket → name it `viam-chartplotter-ecmwf`. Enable the
   public r2.dev URL under Settings → Public access, and add a CORS
   policy so chartplotter browsers can fetch from it:

   ```json
   [
     {
       "AllowedOrigins": ["*"],
       "AllowedMethods": ["GET", "HEAD"],
       "AllowedHeaders": ["*"],
       "ExposeHeaders": ["ETag", "Content-Length"],
       "MaxAgeSeconds": 3600
     }
   ]
   ```

2. **Create a Cloudflare API token** scoped to that bucket with R2 Object
   Read & Write. The token format starting with `cfut_` is the one that
   works with the auto-derive convenience (described below); the older
   "Access Key ID + Secret Access Key" pair from R2 → Manage R2 API Tokens
   also works if you prefer that route.

3. **Add the wind-publisher component** to the chosen machine's Viam
   config. Minimal form (single API token, bucket defaults applied):

   ```jsonc
   {
     "name": "wind-publisher",
     "namespace": "rdk",
     "type": "generic",
     "model": "erh:viam-chartplotter:wind-publisher",
     "attributes": {
       "models": ["ecmwf"],
       "upload_enabled": true,
       "r2_account_id": "<your Cloudflare account ID>",
       "r2_api_token": "cfut_..."
     }
   }
   ```

   Other machines in the fleet should NOT have this component — only one
   publisher per bucket, otherwise multiple machines race to upload the
   same blobs.

4. **Verify the first publish.** Within a few minutes of startup the
   publisher logs should show `publisher: starting ecmwf cycle=… build`
   followed by `publisher: ecmwf cycle=… done in …`. After that:

   ```bash
   curl -s https://pub-<your-hash>.r2.dev/wind/ecmwf/latest.json | jq '{cycle, publishedAt, fhs: .fhs | length, tiles: .tiles | length}'
   # {"cycle": "20260519T06", "publishedAt": "...", "fhs": 49, "tiles": 36}
   ```

5. **Point the consumer chartplotters at the bucket.** The default
   `wind_cdn_base_url` is already the project-wide R2 URL, so nothing to
   change on the consumer side unless you're using a private bucket.

#### Wind-publisher config attributes

| Name                    | Type     | Required | Default | Description |
| ----------------------- | -------- | -------- | ------- | --- |
| `models`                | string[] | yes      | —       | Models to publish. Currently only `["ecmwf"]` is implemented. |
| `upload_enabled`        | bool     | no       | `false` | Off by default so adding the component doesn't immediately write to production. Flip to `true` after credentials are verified. |
| `r2_account_id`         | string   | yes (when `upload_enabled`) | — | Cloudflare account ID. |
| `r2_api_token`          | string   | one of these two | — | Raw Cloudflare API token (single value, `cfut_…`). The Access Key ID is derived via Cloudflare's `/verify` endpoint, and the SigV4 Secret Access Key is computed as SHA-256 of the token value. |
| `r2_access_key_id` + `r2_secret_access_key` | string + string | one of these two | — | Legacy explicit S3 credentials, for setups that already store the derived secret. Either form works. |
| `r2_bucket`             | string   | no       | `viam-chartplotter-ecmwf` | Override only when running a staging fleet against a sandbox bucket. |
| `publish_offset_minutes`| int      | no       | `30`    | Minutes past the hour for the post-cycle wake-up. Tunable only if ECMWF starts publishing earlier. |

#### Manual publish (CLI, for testing or backfill)

The same code path is exposed as a standalone CLI under
`cmd/wind-publisher/`. Useful for one-off publishes, dry-runs against
local disk, or backfilling a cycle the cron loop missed:

```bash
# Dry-run: build a cycle to local disk instead of R2
go run ./cmd/wind-publisher publish --out /tmp/publish-test ecmwf

# Real upload (env vars match the Viam attribute names with R2_ prefix)
export R2_ACCOUNT_ID=...
export R2_API_TOKEN=cfut_...
go run ./cmd/wind-publisher publish --r2 ecmwf

# Inspect the tile grid
go run ./cmd/wind-publisher tiles
```

The CLI shares the same raw-GRIB and tile-blob caches as the in-module
publisher (`~/Library/Caches/viam-chartplotter-wind-publisher/` on
macOS, XDG cache dir on Linux), so re-running after a crash is fast.

#### How consumer chartplotters use the CDN

The chartplotter component reads the CDN URL via its own config attribute
(default points at the project bucket):

| Name                | Type   | Default | Description |
| ------------------- | ------ | ------- | --- |
| `wind_cdn_base_url` | string | `https://pub-6ae2d2a870f74799a963dbc892ea400b.r2.dev` | Base URL the frontend fetches ECMWF tiles from. Empty / unset → uses the project default. |

On boot the frontend probes `<cdn>/wind/ecmwf/latest.json`. If reachable
and < 24 h old, ECMWF becomes the default wind model and tile fetches
go straight to R2. If the CDN is stale (>24 h) the chartplotter falls
back to the local on-demand fetcher (which hits ECMWF directly — useful
as a last-resort but not at fleet scale). If the CDN is unreachable
entirely the chartplotter shows a wind-layer error rather than silently
falling back, to keep CDN outages from cascading 10K direct ECMWF
requests.

### Debug endpoints

| Path                                          | Purpose                                                                                                          |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `/noaa-enc/tile/{z}/{x}/{y}.png`              | The live ENC vector tile the UI consumes. Supports `?sd=N` (draft override, feet), `?style=`, `?navaids=0`, `?landfill=0`, `?skip=…`. |
| `/noaa-enc/compare/{z}/{x}/{y}.png`           | Side-by-side panels — `ours` ‖ `NOAA WMS` ‖ `diff` ‖ `OSM masked` ‖ `mask` — for iterating renderer parity.       |
| `/noaa-enc/compare/test?lat=&lon=`            | HTML page stacking `compare/` panels for the same lat/lon at z=7..16. Defaults to Charleston Harbor.             |
| `/noaa-enc/debug?minLon=&minLat=&maxLon=&maxLat=` | JSON catalog/feature summary for the bbox.                                                                  |
| `/noaa-enc/debug-tile/{z}/{x}/{y}`            | JSON listing of the features painted in a tile, sampled.                                                         |
| `/noaa-enc/stats`                             | Cache and store stats.                                                                                           |

## ToDo

* Options to select color of: tracks, heading line, route line, ais targets and their tracks
* Routes
** See old routes
** save route
** Add: Undo button in case you accidentally delete a route like I just did
** Rum lines and courses
* AIS
** Friends
** Load old AIS tracks from history

## Navigation service

The `erh:viam-chartplotter:nav` model is an `rdk:service:navigation`
implementation backed by an in-memory waypoint list that is mirrored to a JSON
file on disk so waypoints survive module restarts. The chartplotter UI auto-
detects this service, draws the current waypoints as an amber dashed route
from the boat through each waypoint, and exposes buttons to add a waypoint at
the boat's current position, drop one by clicking on the chart, or clear the
whole route.

### Configuration attributes

| Name                | Type   | Required | Description                                                                                                                                                                                                                                  |
| ------------------- | ------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `movement_sensor`   | string | no       | Name of a movement sensor on the same machine. When set, the service's `Location` method reports that sensor's live position and compass heading, and the auto-arrival poller uses it to detect waypoint arrivals.                           |
| `data_path`         | string | no       | Absolute path to the JSON file used to persist waypoints. Defaults to `<user-cache-dir>/viam-chartplotter/nav/<service-name>.json`.                                                                                                          |
| `arrival_radius_m`  | number | no       | When `movement_sensor` is set, the next waypoint is automatically marked visited (and disappears from the route) once the boat is within this many meters of it. Defaults to `200`. Set to a negative number to disable, or omit to use the default. |

### Sample config

```json
{
  "name": "nav",
  "namespace": "rdk",
  "type": "navigation",
  "model": "erh:viam-chartplotter:nav",
  "attributes": {
    "movement_sensor": "gps",
    "data_path": "/var/lib/viam-chartplotter/nav.json",
    "arrival_radius_m": 200
  }
}
```

Both `attributes` fields are optional — the dependency on `movement_sensor`
is reported automatically from the service's config validator, so no
explicit `depends_on` is needed. The minimal config is just `model` plus
`name`/`type`/`namespace`; in that case `Location` returns (0, 0) and
waypoints are written to the default cache path.

## Developing

```
npm install
npm run dev
```

## Building

To create a production version of your app:

```bash
npm run build
```

You can preview the production build with `npm run preview`.

> To deploy your app, you may need to install an [adapter](https://kit.svelte.dev/docs/adapters) for your target environment.
