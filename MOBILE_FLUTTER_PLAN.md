# Mobile (Flutter) Chartplotter — Scoping & Sketch

A plan for a native iOS/Android chartplotter app built entirely in Flutter on
top of [Viam's Flutter SDK](https://flutter.viam.dev/) (`viam_sdk` on pub.dev).
Goal: the core "where am I, what's around me, what's the weather, how deep is it"
experience from the web app, on a phone/tablet, talking to the same boat and the
same tile/weather server.

---

## 1. What the web app actually does (the thing we're porting)

The current app is a Svelte + OpenLayers single-page app (`src/App.svelte`,
`src/marineMap.svelte`) with two independent connections:

1. **Boat connection** — a Viam `RobotClient` over WebRTC
   (`VIAM.createRobotClient`, signaling via `app.viam.com`). A 1 Hz loop
   (`doUpdate`) polls components by name and pushes the readings into reactive
   state:
   - `movement_sensor` → position, linear velocity (SOG), compass heading, COG
   - `sensor` (by name regex) → depth, sea temp, wind, SpotZero water flow,
     Seakeeper, AC/Victron power, fuel/freshwater gauges
   - `sensor` (`ais`, `airstream`) → AIS targets via `getReadings` /
     `DoCommand({command:"all_history"|"history"})`
   - `navigation` service → waypoints (`getWayPoints`)
   - `camera` → `getImages` rendered into `<img>` blobs
   - `client.resourceNames()` / `getMachineStatus()` → resource discovery
2. **Cloud connection** — a `ViamClient` (`VIAM.createViamClient`) used for:
   - `appClient.getRobotPart(...)` → the machine's config JSON (used to find
     per-component attributes like `chartplotter-hide`, and remote credentials)
   - `dataClient.tabularDataByMQL(...)` → historical sparklines (gauges, sea
     temp) and the recorded position/depth track, via BSON-serialized MQL
     pipelines, with a hot→cold store fallback.

**The map itself is rendered server-side.** The Go module
(`module.go`, `render/handlers.go`, `weather/`) exposes a read-only HTTP API the
frontend consumes as plain raster/vector tiles:

| Endpoint | Content |
|----------|---------|
| `/noaa-enc/tile/{z}/{x}/{y}.png` | NOAA ENC vector charts rendered to PNG (depth-shaded by draft; `?` style params) |
| `/noaa-enc/osm-tile/{z}/{x}/{y}.png` | OSM land underlay |
| `/noaa-enc/navaids`, `/noaa-enc/structures` | GeoJSON aids-to-navigation / structures |
| `/noaa-weather/data/...`, `/noaa-weather/models` | wind/wave/isobar/satellite weather as JSON/GeoJSON |
| `/noaa-wms/proxy` | NOAA official WMS passthrough |
| Esri World Imagery (external XYZ) | satellite/aerial base |
| `/app-config`, `/version`, `/myboat-icon` | runtime config, hot-reload signal, boat icon |

The OpenLayers map stacks: one **raster base layer** (OSM / satellite / NOAA
ENC / NOAA-ECDIS / compare) + **vector overlays** (navaids, structures, AIS
boats, my-boat marker, waypoints, active route, route previews, measure tool) +
**weather overlays** (animated wind particles via `ol-wind`, isobar lines, wave,
satellite cloud).

**The single most important consequence for mobile:** the chart rendering is
*already done on the server and shipped as XYZ raster tiles.* A Flutter map
widget can point straight at the same `{z}/{x}/{y}.png` URLs. We do **not**
re-implement ENC rendering on the phone.

---

## 2. Target architecture (all Flutter)

```
┌──────────────────────────── Flutter app ─────────────────────────────┐
│                                                                       │
│  flutter_map (Leaflet-style)            viam_sdk                       │
│   ├─ TileLayer  ── HTTP ─────────────►  tile/weather server (Go, EXISTING, unchanged)
│   │    /noaa-enc/tile, osm-tile, esri, /noaa-weather/...               │
│   ├─ MarkerLayer / PolylineLayer  ◄── app state (boat, AIS, waypoints, routes)
│   └─ CustomPainter (wind particles, isobars)                          │
│                                                                       │
│  Riverpod/Bloc state  ◄── 1 Hz poll loop                              │
│   ├─ RobotClient (WebRTC)  ─────────►  the boat (movement_sensor,     │
│   │                                     sensors, camera, navigation)  │
│   └─ ViamClient (app + data) ───────►  app.viam.com (config, MQL history)
└───────────────────────────────────────────────────────────────────────┘
```

Reuse boundary:
- **Reuse as-is:** the entire Go tile/weather/data server. Mobile is just
  another HTTP client of it. Zero backend work for chart/weather pixels.
- **Reuse the protocol, re-implement the client:** the boat poll loop and the
  cloud MQL queries — same component names, same MQL pipelines, rewritten in
  Dart against `viam_sdk`.
- **Rebuild in Flutter:** all UI — map widget, overlays, data panel, gauges,
  routes panel, camera tiles.

Recommended stack: `flutter_map` (raster XYZ + marker/polyline layers, mature,
free), `viam_sdk` (boat + cloud), `riverpod` (state), `fl_chart` (sparklines).

---

## 3. Component-by-component port map

| Web piece | Flutter equivalent | Effort | Notes |
|-----------|-------------------|--------|-------|
| Raster base tiles (OSM/ENC/satellite/ECDIS) | `flutter_map` `TileLayer` w/ same URL templates | **Low** | Pure URL reuse. Pass the same query params (draft/style). |
| My-boat marker + heading/COG | `Marker` w/ rotated icon | Low | `/myboat-icon` or bundled asset. |
| AIS targets + popups | `MarkerLayer` + bottom sheet | **Med** | Many markers → cluster/cull; reuse `mmsi.ts`/`BoatInfo` logic in Dart. |
| AIS history tracks | `PolylineLayer` from `DoCommand("history")` | Med | Same DoCommand contract. |
| Navaids / structures GeoJSON | fetch + `PolygonLayer`/`MarkerLayer` | Med | Parse GeoJSON (`geojson` pkg). |
| Waypoints + active route | `NavigationClient.getWayPoints` + `PolylineLayer` | Med | Editing/reordering UI is the work, not the data. |
| Saved-route preview + capture | port `routeStore.ts` to Dart | Med | Pure logic; has tests to mirror. |
| Data panel (speed/depth/wind/temp…) | Flutter widgets bound to poll state | Low–Med | Direct rewrite of the `doUpdate` readings. |
| Historical sparklines (gauges, temp, depth, track) | `dataClient.tabularDataByMql` + `fl_chart` | **Med** | MQL pipelines port directly; BSON via `bsonfy`→Dart `bson`. |
| Camera tiles | `viam_sdk` camera widget / `getImage` loop | Med | Streaming widget exists; watch bandwidth on cellular. |
| **Wind particle animation** | custom `CustomPainter` / shader | **High** | No off-the-shelf `flutter_map` equivalent to `ol-wind`. |
| Isobars / wave / satellite cloud overlays | `CustomPainter` / image overlay | Med–High | Isobars = polylines; satellite = image overlay; wave = colored grid. |
| Connection/host entry (URL+cookie today) | machine picker via `appClient` or QR/deep link | **Med** | Web inherits host from URL/cookie; mobile must build real onboarding. |
| Hot-reload via `/version` poll | drop it | — | Not needed on mobile. |

---

## 4. Hard parts (ranked)

### 4.1 Wind-particle weather animation — *hardest* (DEFERRED past v1, see §6)
`ol-wind` gives the flowing wind streamlines essentially for free in OpenLayers.
There is no equivalent for `flutter_map`. Options, cheapest first:
1. **Static barbs/arrows** sampled on a grid (a `CustomPainter` over the map) —
   covers 80% of the value, low risk. Recommended for v1.
2. **Animated particles** in a `CustomPainter` with a particle pool advected by
   the wind field (re-implement the ol-wind algorithm). Doable but real work and
   a battery/perf concern on a 1 Hz-updating map.
3. **GPU shader** (`FragmentShader`) for true ol-wind-quality flow — best
   looking, most effort.
Decouple this: ship v1 with static wind, treat animation as a follow-up.

### 4.2 Map interaction parity
OpenLayers gives precise pan/zoom, rotation, click-to-identify, a measure tool,
and per-layer opacity for free. `flutter_map` covers pan/zoom/rotate and
marker/polyline hit-testing, but the measure tool, click-to-identify against
vector overlays, and smooth multi-layer opacity blending are all hand-built.
Plan for a few hundred lines of gesture/hit-test code.

### 4.3 Connectivity, auth & onboarding — *main net-new build for v1*
**Decided: full `app.viam.com` login (§6).** 
The web app is *handed* its host + credentials via URL params / cookies /
`userToken`, and opens both a WebRTC `RobotClient` and a cloud `ViamClient`.
A mobile app has no such ambient context and must own:
- **Auth**: API-key (simplest, store in secure storage) or an OAuth/login flow
  for `app.viam.com`. The web `userToken` cookie path doesn't exist on mobile.
- **Machine discovery**: list the user's machines/locations via `appClient` and
  let them pick the boat (or scan a QR / open a deep link).
- **WebRTC on mobile**: `viam_sdk` uses `flutter_webrtc` under the hood — works
  on iOS/Android but needs camera/mic-style permission plumbing in some setups,
  and reconnection/`SESSION_EXPIRED` handling must be reimplemented (the web app
  has a hand-rolled reconnect in `errorHandler`).

### 4.4 SDK feature-parity verification (beta risk)
`viam_sdk` is in **beta** ("breaking changes may occur in patch versions"). The
features we depend on need a spike to confirm parity:
- ✅ `RobotClient` over WebRTC, sensor/movement_sensor/camera/navigation clients,
  `DoCommand` — supported.
- ✅ `dataClient.tabularDataByMql(orgId, pipeline)` — supported (BSON pipeline).
- ⚠️ `appClient.getRobotPart(...).configJson` — used to read per-component
  attributes (`chartplotter-hide`) and **remote credentials** for cross-org data
  scoping (`dataClientForComponent`). Verify this is exposed in Dart; if not,
  the remote-history feature degrades to host-only (acceptable for v1).
- ⚠️ `getCloudMetadata()` / `getMachineStatus()` shapes — used for data scoping
  (org/location/robot IDs). Confirm field availability.

### 4.5 Battery / network on the water
1 Hz polling + camera frames + tile fetching is fine on shore Wi-Fi, rough on
cellular/satellite at sea. Needs: adaptive poll rates, tile caching to disk
(`flutter_map` cache plugins), pause-when-backgrounded, and a cell-data budget.
The web app never had to care about this; the mobile app must.

### 4.6 Offline charts — *DEFERRED past v1 (§6); online-only to start*
Biggest *product* gap, not strictly required for v1 but expected of a "real"
chartplotter: phones lose signal offshore. Server-rendered tiles assume
connectivity. True offline needs either pre-fetched tile bundles (download a
region before leaving the dock) or on-device ENC rendering (large new effort —
avoid). Recommend region pre-fetch as a fast-follow.

---

## 5. Phased plan

- **Phase 0 — Spike (½–1 wk):** new Flutter app; `viam_sdk` connect to the boat
  with a hard-coded API key; `flutter_map` showing `/noaa-enc/tile` + boat
  marker from live `movement_sensor`. Confirms the two riskiest integrations
  (WebRTC connect + tile reuse) in one shot.
  → **Scaffolded in [`mobile/`](mobile/)** (see `mobile/README.md`). Code is
  written against the documented `viam_sdk` ~0.3 surface; still needs a Flutter
  toolchain + real boat creds to compile and validate on hardware (§4.4 parity
  checks).
- **Phase 1 — Core nav (2–3 wk):** poll loop → data panel (speed/heading/depth/
  wind/temp); base-layer switcher (OSM/ENC/satellite); AIS targets + popups;
  navaids/structures overlays; onboarding (machine picker + secure-stored API
  key).
- **Phase 2 — Routes & history (2 wk):** waypoints/active route via
  `NavigationClient`; port `routeStore` + saved-route preview; MQL sparklines
  (gauges/temp/depth) and recorded track via `tabularDataByMql`.
- **Phase 3 — Weather & camera (2 wk):** static wind barbs + isobars + satellite
  overlay; camera tiles. Animated wind = stretch.
- **Phase 4 — Marine hardening:** tile caching, region pre-fetch (offline),
  adaptive polling, reconnection robustness, background behavior.

Rough order-of-magnitude: a usable v1 (Phases 0–2) in ~6–8 focused weeks for one
engineer; weather animation and true offline are the long-tail items.

---

## 6. Product decisions (locked) & remaining questions

Decided:

1. **Platform — both, phone-first.** Responsive layout; optimize the handheld
   case first, let tablets use the extra screen. → favors a single adaptive
   layout (`LayoutBuilder`/breakpoints), not two codebases. Design the data
   panel to collapse on phones and expand into a helm dashboard on tablets.
2. **Auth — full `app.viam.com` login.** OAuth against Viam cloud, then list the
   user's locations/machines and let them pick the boat. → onboarding is real
   work (Phase 1); supports multiple users/guests, not just the owner. The web
   app's API-key/cookie path is *not* the model.
3. **Offline — online-only for v1.** Ship needing connectivity. Tile disk
   caching is still worth it for speed/flakiness, but region pre-fetch and
   on-device rendering are explicitly **out of scope for v1** (fast-follow).
4. **Weather — static wind barbs + isobars for v1.** Removes the hardest item
   (§4.1) from the critical path: grid-sampled barbs + isobar polylines in a
   `CustomPainter`. Animated particles become a post-v1 stretch.

Still open:

5. **Camera priority** — important on mobile, or drop it for v1 to save
   bandwidth/effort? (Leaning: include but behind a tap, not auto-streaming.)
6. **Codebase** — new repo or a `mobile/` dir in this repo? Recommended:
   `mobile/` here, so it lives next to the server contract it depends on.

These decisions shrink v1's risk: the two hardest items (wind animation, offline
charts) are both deferred, leaving **Viam-login onboarding** as the main net-new
build beyond the straight UI port.

---

*Sketch only — no app code written yet. The recommended next concrete step is
Phase 0: a throwaway spike proving `viam_sdk` WebRTC connect + `flutter_map`
tile reuse against a real boat. With weather animation and offline deferred,
Phase 1's onboarding (Viam OAuth + machine picker) is the main new risk to
de-risk early.*
