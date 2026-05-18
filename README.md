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
