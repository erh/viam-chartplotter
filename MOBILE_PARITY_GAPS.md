# Web ↔ Mobile Parity — Gap List & Plan

What the Svelte/OpenLayers web app (`src/`) does that the Flutter app
(`mobile/`) does not, and a sequenced plan to close it.

Companion to [`MOBILE_FLUTTER_PLAN.md`](MOBILE_FLUTTER_PLAN.md), which scoped
the port before any Dart existed. This document is the *post-Phase-0* audit:
the spike shipped more than its README claims (AIS, wind, cameras, tanks,
graphs, OAuth login all landed), so the remaining work is re-derived from the
code as it stands today, not from the original phase plan.

Both apps talk to the same Go module (`module.go`, `render/`, `weather/`,
`nav_routes.go`) over the same HTTP + Viam RPC contracts. **Nothing in this
document requires backend work** unless explicitly marked.

Code read for this audit:

| Side | Files | Lines |
|------|-------|-------|
| Web | `src/App.svelte`, `src/marineMap.svelte`, `src/lib/*` | ~15,300 |
| Mobile | `mobile/lib/**/*.dart` | ~3,400 |

---

## 1. Scorecard

| Area | Web | Mobile | State |
|------|-----|--------|-------|
| Base chart tiles | 6 bases, zoom-tiered render params, US-waters OSM fallback | 4 flat XYZ layers, fixed params | ◑ partial |
| Vector chart overlays | navaids, structures, areas + S-57 styling & tooltips | none (tile-baked only) | ○ missing |
| Own boat & track | scaled icon, live track, depth colouring, heading line, recorded track | plain arrow marker | ◑ partial |
| AIS | tracks, projections, CPA/TCPA, flags, fleet sources, boats panel | markers + detail sheet | ◑ partial |
| ADS-B aircraft | full layer + projections + detail cards | none | ○ missing |
| Routes & waypoints | full CRUD, drag-edit, saved routes, GPX, save-from-track | read-only destination line | ○ missing |
| Weather overlays | animated wind, waves, isobars, lightning, satellite, model picker | static wind arrows, GFS only | ◑ partial |
| Tides / local forecast | NOAA tides, open-meteo, sun & moon | none | ○ missing |
| Data panel & gauges | grouped gauges, historical sparklines, hover freshness | drawer + sparklines + graphs | ● near parity |
| Systems (AC/Victron/Seakeeper) | detail tables, yacht page, Seakeeper control | 2 summary rows, read-only | ◑ partial |
| Cameras | inline grid + full-screen enlarge | separate 1 Hz screen | ◑ partial |
| Map interaction | measure, auto-zoom, pan-mode, tile debug, layers panel | pinch/zoom, course-up | ◑ partial |
| Persistence | layer states, view, tool settings in localStorage | nothing persisted | ○ missing |
| Auth & onboarding | URL/cookie handed in | **OAuth + machine picker** | ▲ mobile ahead |
| Marine hardening | n/a (shore browser) | reconnect watchdog only | ○ missing |

● parity ◑ partial ○ missing ▲ mobile ahead of web

---

## 2. Gap list

Effort key: **S** ≤1 day · **M** 1–3 days · **L** ~1 week · **XL** >1 week.
Priority: **P0** helm-critical · **P1** expected of a chartplotter · **P2** nice to have.

### A. Chart & base layers

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| A1 | **Zoom-tiered tile params.** Web requests three different renders by zoom: `style=ecdis` overview (z<12), `style=wms&navaids=0` mid (12–13), plus `skip=BRIDGE,CBLOHD,PIPOHD,CONVYR` at detail (z≥14). Mobile pins `style=ecdis&landfill=0` at every zoom, so the detail chart is wrong and `landfill=0` is a no-op that forks the server tile cache into a redundant shard. | `marineMap.svelte:3738-3789` | S | P0 |
| A2 | **Safe-depth (`sd`) param** — depth shading tuned to the boat's draft. Mobile never sends it. | `marineMap.svelte:344,3629` | S | P0 |
| A3 | **Tile version cache-buster** (`v=tileGenVersion`); mobile can serve stale tiles indefinitely after a server render change. | `marineMap.svelte:3628` | S | P1 |
| A4 | **Missing bases**: OpenSeaMap, NOAA official WMS (`/noaa-wms/proxy`), NOAA-ECDIS. | `marineMap.svelte:3603,3835,3859` | M | P2 |
| A5 | **Under-chart OSM fallback.** Web keeps OSM beneath the chart and suppresses it only for tiles fully inside US ENC coverage, so foreign/uncharted waters still render. Mobile shows one layer at a time → blank chart outside US coverage. | `marineMap.svelte:3547-3562`, `tileFullyInUSWaters:121` | M | P0 |
| A6 | **Runtime `/app-config`** (`tileServerBaseURL`, `chartOnly`). Mobile hardcodes `TILE_BASE` at build time via `--dart-define`, so it cannot follow a boat's own tile server or enter chart-only mode. | `marineMap.svelte:3509`, `module.go:374` | M | P1 |

### B. Vector chart overlays

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| B1 | **Navaids layer** — GeoJSON from `/noaa-enc/navaids`, S-57 colour decoding, synthesised buoy/beacon icons, light-characteristic labels, tap tooltips. | `marineMap.svelte:1512-1960,3802` | L | P0 |
| B2 | **Structures layer** — bridges/cables/pipelines/conveyors with clearance tooltips, paired with A1's `skip` param. | `marineMap.svelte:1991-2127,3818` | M | P1 |
| B3 | **Areas** from `area` components: normalised GeoJSON, folder grouping, date-range visibility. | `App.svelte:1072-1147`, `marineMap.svelte:893-951,4176` | M | P2 |
| B4 | **bbox load strategy + feature-cap eviction** (3000 features) so a long coastal session doesn't grow unbounded. Needed by B1/B2. | `marineMap.svelte:3640-3660` | S | P1 |

### C. Own boat & track

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| C1 | **Boat icon**: custom `/myboat-icon` with length/beam-scaled axes. Mobile draws a generic red arrow. | `marineMap.svelte:165,3025,3056` | S | P1 |
| C2 | **Live track line** with per-point depth/speed and a depth-colour toggle. | `marineMap.svelte:2696-2900` | M | P0 |
| C3 | **Recorded track from the cloud** (`positionHistoryMQL`, windowed fetch, hot→cold fallback, GPS-alternate names). Mobile backfills scalar metrics but has no position history at all. | `App.svelte:1521-1608,1847-1935` | M | P1 |
| C4 | **Heading line** with user-set length in nm. | `marineMap.svelte:3328-3376,3961` | S | P1 |
| C5 | Track pruning / fleet-scale render budget. | `marineMap.svelte:1463` | S | P2 |

### D. AIS & other traffic

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| D1 | **AIS history tracks** via `DoCommand{all_history|history}`, incl. per-vessel fetch on popup open. | `App.svelte:565-637` | M | P1 |
| D2 | **Projection (ahead) vectors** with a minutes selector. | `marineMap.svelte:377-391,4101` | S | P1 |
| D3 | **CPA / TCPA** in the vessel popup — the one genuinely safety-critical AIS readout. | `marineMap.svelte:5147-5180` | S | P0 |
| D4 | **Country flag from MMSI** (MID table, ~250 entries). | `src/lib/mmsi.ts` | S | P2 |
| D5 | **Length-scaled AIS triangles** instead of one fixed icon. | `marineMap.svelte:2545-2637` | S | P2 |
| D6 | **Extra AIS sources**: `ais-web-sender` components (fleet) and `airstream` with viewport-bbox DoCommand + debounce. | `App.svelte:728-780,1049-1071` | L | P2 |
| D7 | **Boats panel**: searchable list, per-boat visibility, select/deselect all, offline boats, fit-all-visible. | `marineMap.svelte:474-500,613` | M | P2 |
| D8 | **ADS-B aircraft** layer, projections, sparse-field detail cards. | `App.svelte:532-546`, `marineMap.svelte:2145-2440` | M | P2 |
| D9 | **Detections overlay** (vision detections on the chart). | `marineMap.svelte:2652-2695` | M | P2 |
| D10 | **AIS render budget** — mobile builds an unbounded `MarkerLayer` from every target each 5 s poll; no viewport cull or clustering. Busy harbour = frame drops. | mobile `map_screen.dart` | S | P0 |

### E. Routes & waypoints

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| E1 | **Nav waypoint CRUD** — add / insert-between / drag-move / remove / clear against the navigation service, with map-side drag-modify and insert/delete popups. Mobile can only *view* the active destination. | `App.svelte:2241-2348`, `marineMap.svelte:3122-3311` | L | P0 |
| E2 | **Saved routes** — the `routes_*` DoCommand surface: list/save/rename/delete, load, load-reversed, colour, preview-on-map, parent-location (read-only) scope. | `src/lib/routeStore.ts`, `RoutesPanel.svelte`, `nav_routes.go` | L | P1 |
| E3 | **Save-from-track**: pull a recorded track window, Douglas-Peucker simplify, save as a route. | `RoutesPanel.svelte:570-640`, `src/lib/simplify.ts` | M | P1 |
| E4 | **GPX export** (carry a route to a Garmin/SD card). | `src/lib/gpx.ts` | S | P2 |
| E5 | **Route stats**: next *and* final waypoint distance/ETA, distance-along-line. Mobile shows next-waypoint only. | `marineMap.svelte:556-612` | S | P1 |

### F. Weather overlays

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| F1 | **Animated wind particles** (ol-wind). Mobile has static arrows — the deliberate v1 deferral, still open. | `marineMap.svelte` dynamic import | L | P2 |
| F2 | **Model picker** from `/noaa-weather/models` with availability reasons. Mobile is GFS-only. | `WeatherOverlays.svelte:1030-1082` | M | P1 |
| F3 | **Waves** overlay + colour scale + legend. | `WeatherOverlays.svelte:483`, `src/lib/windLayer.ts` | M | P1 |
| F4 | **Isobars** (pressure contour polylines). | `src/lib/isobarLayer.ts`, `WeatherOverlays.svelte:495` | M | P2 |
| F5 | **Lightning** (nowCOAST). | `WeatherOverlays.svelte:506` | S | P2 |
| F6 | **Satellite cloud imagery** — NASA GIBS colour/visible/infrared variants. | `WeatherOverlays.svelte:316-334,518` | M | P2 |
| F7 | **Point weather sample** (web: cursor readout of wind/wave; mobile equivalent = long-press to sample). | `marineMap.svelte` cursorInfo | S | P2 |
| F8 | **Zoom gates** hiding 0.25° fields at detail zoom where a cell spans the screen. | `LayerOption.maxZoom` | S | P1 |

### G. Environment info

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| G1 | **Tides** — NOAA station list, nearest-station pick, predictions, next high/low, sparkline, interpolated now-level, station link. | `marineMap.svelte:5281-5550` | M | P0 |
| G2 | **Local forecast** (open-meteo): air temp, wind, rain total, sunrise/sunset. | `WeatherOverlays.svelte:746` | M | P1 |
| G3 | **Moonrise / moonset** (SunCalc port). | `src/lib/moon.ts` | S | P2 |
| G4 | **Windy.com deep link** for the boat's position. | `marineMap.svelte:6464` | S | P2 |

### H. Data panel, gauges, history

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| H1 | **Gauge grouping + capacity in gallons + level colour thresholds.** Mobile shows bare percentages. | `App.svelte:2145-2187`, `helpers.ts` | S | P1 |
| H2 | **Sparkline hover** → value + "how long ago", and the short/long graph-range toggle. Mobile has sparklines and a detail graph but neither affordance. | `App.svelte:1979-1990,2219-2240` | S | P2 |
| H3 | **AC / Victron detail**: per-line volts/amps table, Victron power components, door sensors, and the Yacht Details page. Mobile shows one summary row. | `App.svelte:389-404,1023-1030`, `YachtDetails.svelte` | M | P2 |
| H4 | **Seakeeper control** — power/stabilize on/off DoCommand. Mobile is read-only. *(The only write action in either app besides waypoints.)* | `App.svelte:2349-2368` | S | P1 |
| H5 | **Remote-part data scoping** — read the machine config, extract remote credentials, build a per-remote `ViamClient` so history works for components borrowed from another org. Mobile is single-org. | `App.svelte:1623-1767` | M | P2 |
| H6 | **`chartplotter-hide` attribute** honoured when listing components/cameras. Mobile ignores it (noted as TODO in `viam_connection.dart`). | `App.svelte:838` | S | P1 |
| H7 | **Movement-sensor alternates** — query history across every GPS name the boat has had. | `App.svelte:1192,1529-1560` | S | P2 |

### I. Cameras

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| I1 | **Tap-to-enlarge** full-screen view with pinch-zoom (web: enlarge + Esc). | `App.svelte:2369-2400` | S | P1 |
| I2 | **Stale/failed camera handling** — drop a camera after repeated failure, show last-frame age. | `App.svelte:877-894` | S | P2 |

### J. Map interaction & UX

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| J1 | **Measure tool** (click-to-click distance/bearing). | `marineMap.svelte:3102-3120,3381-3429` | S | P1 |
| J2 | **Boat position on screen** (centre vs bottom, for look-ahead). | `marineMap.svelte:3318-3327` | S | P1 |
| J3 | **Auto-zoom** with speed. | `marineMap.svelte:3450-3459` | S | P2 |
| J4 | **Pan mode + "Stop Panning"** — manual pan suspends follow, one tap re-anchors. Mobile recentres only on first fix and via the FAB, and never auto-follows. | `marineMap.svelte:3430-3449,4229` | S | P0 |
| J5 | **Layers panel** with hierarchy, per-layer sub-toggles and auto-hide. Mobile has a 4-item dropdown that won't scale past ~6 layers. | `marineMap.svelte:186-236` + panel markup | M | P1 |
| J6 | **Persistence** of layer states, view centre/zoom, heading-line length, projection minutes (localStorage). Mobile persists nothing — every launch resets to Long Island Sound at z9. | `marineMap.svelte:292-420` | S | P0 |
| J7 | **Scale line** control. | `marineMap.svelte` ScaleLine | S | P1 |
| J8 | **Tile-URL debug mode** + render-compare links. | `marineMap.svelte:3460-3499` | S | P2 |

### K. Connection & configuration

| # | Gap | Web reference | Effort | Pri |
|---|-----|--------------|--------|-----|
| K1 | **Machine/resource health surfacing** — web reads `getMachineStatus()` and shows per-component status; mobile only shows a connection chip. | `App.svelte:1609-1622` | S | P1 |
| K2 | **Discovery breadth** — web also finds `adsb`, `airstream`, AIS web senders, Victron powers, door sensors, `area` components and the nav service. Rolls up with the features above. | `App.svelte:936-1147` | — | — |
| K3 | `/version` hot-reload — deliberately dropped on mobile. | `App.svelte:58` | — | n/a |

### L. Marine hardening (mobile-only; no web counterpart)

These have no web equivalent — the browser runs at the dock. They are still
gaps between *the mobile app and a usable chartplotter*.

| # | Gap | Effort | Pri |
|---|-----|--------|-----|
| L1 | Tile disk cache (`flutter_map` cache store). | M | P0 |
| L2 | Offline region pre-fetch ("download this area before leaving"). | L | P1 |
| L3 | Adaptive poll rate, pause-when-backgrounded, cellular data budget. | M | P0 |
| L4 | Night mode (red-on-black) + keep-screen-awake helm mode. | M | P1 |
| L5 | Device-GPS fallback when the boat GPS is unavailable/disconnected. | S | P1 |
| L6 | Tablet vs phone adaptive layout (locked decision in the plan, not yet built). | M | P1 |
| L7 | Test coverage — mobile has 2 test files vs the web's suite (`gpx`, `moon`, `routeStore`, `simplify`) plus the Go tests. | M | P1 |

---

## 3. Where mobile is ahead of web

Worth back-porting or at least not regressing:

1. **`app.viam.com` OAuth login + org/location/machine picker** (`mobile/lib/auth/`, `screens/`). The web app is handed its host by URL/cookie and has no login of its own.
2. **Dead-connection watchdog** — bounded RPC timeouts, a hung-tick detector, re-dial on resume from lock (`viam_connection.dart`). The web reconnect is a 30 s error-string heuristic.
3. **Fuel mode with dual freshness** — separates "when the app last fetched" from "when the boat's CAN bus last had data" (`_last_update`), and flags a lying clock rather than trusting it (`fuel_screen.dart`).
4. **Windowed detail graphs** (15 m / 1 h / 4 h / all) with cloud backfill on open.

Also: **`mobile/README.md` is out of date** — it says AIS, weather, routes,
camera and history are "deliberately not yet" done; all but routes now exist.
Fix as part of M0.

---

## 4. Plan

Sequenced so each milestone leaves the app *shippable*, and so the P0 gaps —
the ones that make the mobile app unsafe or annoying to actually navigate
with — land first. Estimates are for one engineer.

### M0 — Correctness quick-wins (2–3 days)
**A1, A2, A3, D10, J6, H6** + README fix.

Small, high-leverage, no new subsystems: the chart renders what the web chart
renders, tiles aren't stale, AIS stops melting the frame budget in a harbour,
and the app reopens where you left it. *Exit:* a side-by-side screenshot at
z9/z13/z15 matches the web chart; 300 AIS targets hold 60 fps.

### M1 — Helm fundamentals (1.5 wk)
**A5, A6, C1, C2, C4, J1, J2, J4, J7, K1, L5.**

Follow-the-boat behaviour, a real boat icon, the track behind you, heading
line, measure, scale, and a tile base that follows the machine's config.
*Exit:* a full run out and back is navigable without touching the web app.

### M2 — Safety overlays (1.5 wk)
**B1, B2, B4, D3, D1, D2, G1.**

Navaids and structures as tappable vectors, CPA/TCPA on AIS targets, AIS
tracks and projections, tides. This is the milestone that makes it a
chartplotter rather than a boat dashboard. *Exit:* every navaid on the chart
is tappable with its light characteristic; a crossing target shows CPA/TCPA.

### M3 — Routes & waypoints (2 wk)
**E1, E2, E5, E3, E4.**

The largest single missing subsystem and the last P0. Port `routeStore.ts` to
Dart with its tests mirrored, then build waypoint editing (long-press to add,
drag to move, tap-to-insert/delete) and the routes sheet.
*Exit:* a route created on the phone loads on the web app and vice versa.

### M4 — Marine hardening (1.5 wk)
**L1, L3, L4, L6, C3.**

Tile disk cache, adaptive polling and background pause, night mode,
tablet layout, recorded track from the cloud. *Exit:* a 4 h offshore run on
cellular stays usable and inside a stated data budget.

### M5 — Weather & environment (2 wk)
**F2, F3, F8, G2, G3, G4, F4, F7.**

Model picker, waves, zoom gates, local forecast, sun/moon. Isobars if the
`isobarLayer.ts` port goes cleanly. *Exit:* the weather sheet answers "what's
it doing here in 6 hours" without leaving the app.

### M6 — Systems, cameras, panel polish (1 wk)
**H1, H2, H4, I1, I2, J5, J3, J8.**

Gauge grouping and gallons, Seakeeper control, camera enlarge, a layers panel
that scales past six entries.

### M7 — Long tail (as demanded, 2–3 wk)
**F1** animated wind · **B3** areas · **D6** airstream/web senders · **D7**
boats panel · **D8** ADS-B · **D9** detections · **A4** extra bases · **D4,
D5** flags and scaled triangles · **H3** Victron/yacht page · **H5** remote
data scoping · **H7** GPS alternates · **L2** offline pre-fetch.

Everything here is either fleet/tinkerer-facing or explicitly deferred; pull
items forward only on demand.

**Timeline:** P0 complete end of M3 (~5.5 wk). Full parity minus the long tail
~10 wk. Long tail takes it to ~13 wk.

### Cross-cutting work (fold into the milestones above)

1. **Port the pure logic, mirror the tests.** `mmsi.ts` (D4), `simplify.ts`
   (E3), `gpx.ts` (E4), `moon.ts` (G3), `routeStore.ts` (E2), `computeCpa`
   (D3) are all dependency-free and already have Vitest suites — port each
   with its tests to Dart so the two clients can't drift silently. (L7)
2. **One reading-parse layer.** Sensor key spellings (`Course Over Ground` /
   `cog` / `COG`, `Sog`/`SOG`/`Speed`) are duplicated in both apps and already
   differ in coverage. Centralise per side and cover with table tests.
3. **A parity checklist in CI.** A markdown table keyed by the gap IDs here,
   updated per PR, so "what's still missing" doesn't need re-deriving.
4. **Extend the mobile CI** (`.github/workflows/mobile-flutter.yml`) to run
   `flutter test` alongside `analyze`, once M0 lands.

### Risks

- **Vector-overlay volume (B1/B2).** OpenLayers' bbox strategy + feature cap
  has no `flutter_map` equivalent; expect to hand-roll load-by-viewport and
  eviction. Budget the full **L**, and test in a busy harbour.
- **Waypoint editing UX (E1).** Drag-to-move and tap-to-insert are mouse
  idioms; the touch design (long-press, drag handles, undo) is real design
  work, not just a port.
- **`viam_sdk` beta drift.** E1/E2 depend on `NavigationClient` DoCommand and
  H5 on `getRobotPart(...).configJson` — verify both are exposed in Dart
  before committing to M3's estimate.
- **Battery/data (L3).** 1 Hz polling plus tiles plus camera frames is a shore
  Wi-Fi budget. Measure before offshore testing, not after.
