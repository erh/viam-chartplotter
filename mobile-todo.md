# Mobile TODO — web→Flutter parity backlog

The actionable plan for closing the gaps catalogued in
[`MOBILE_PARITY_GAPS.md`](MOBILE_PARITY_GAPS.md). That document is the *why*
(what the web app does, where, and how far behind mobile is); this one is the
*what to do*, written so a single task can be handed to one agent with no
further context.

- **One card = one PR.** Cards are sized so the whole thing fits in one
  session. Cards inside a milestone are mostly independent; `Depends on:`
  names the exceptions.
- **Task IDs are stable** (`A1`, `E2`, …) and match the gap IDs in
  `MOBILE_PARITY_GAPS.md`. Cite them in branch names and PR titles.
- **Priority**: `P0` helm-critical · `P1` expected of a chartplotter ·
  `P2` nice to have. **Effort**: `S` ≤1 day · `M` 1–3 d · `L` ~1 wk.

---

## Working agreement (read before picking up a card)

**Layout.** Web app: `src/` (Svelte 5 runes + OpenLayers + TypeScript). Mobile:
`mobile/lib/` (Flutter + `flutter_map` + `viam_sdk`). Shared backend: the Go
module at the repo root (`module.go`, `render/`, `weather/`, `nav*.go`).

**Build & test.**

```
make mobile-analyze    # flutter analyze   (CI gate)
make mobile-test       # flutter test      (CI gate)
make mobile-run        # chart-only, no creds
make mobile-run VIAM_OAUTH_ISSUER=… VIAM_OAUTH_CLIENT_ID=…   # login path
make mobile-run VIAM_HOST=… VIAM_API_KEY_ID=… VIAM_API_KEY=… # API-key path
```

CI (`.github/workflows/mobile-flutter.yml`) runs `flutter analyze`, `flutter
test` and a debug APK build with no credentials — **every card must leave the
app compiling and running chart-only**.

**Ground rules.**

1. **Don't change the Go server.** Every card here is client-side. If you
   believe a card needs a backend change, stop and say so in the PR — the two
   clients share these endpoints and the web app must not regress.
2. **The web app is the contract, not the gospel.** Match its *data* handling
   exactly (units, key spellings, endpoint params, fallbacks). Do not copy its
   *interaction* design — mouse hover and right-click have no touch equivalent;
   each card says what the touch idiom should be.
3. **Port pure logic with its tests.** Anything under `src/lib/` with a
   `.test.ts` beside it gets ported to Dart *with the test cases translated*.
   That's how the two clients stay from drifting.
4. **Units are SI on the wire, display units in the UI.** Sensors report metres,
   °C, m/s; the UI shows feet, °F, knots. Conversion constants live in one place
   per side — reuse, don't re-type them.
5. **Never block the poll loop.** New network work goes on its own cadence, not
   inside `ViamConnection._tick()` (see `mobile/lib/viam_connection.dart`, which
   already staggers AIS/route/systems at `_tickN % 5`).
6. **Update the card** — tick the acceptance boxes in your PR, and flip the
   milestone table row when the last card in a milestone lands.

**Sensor-reading key spellings** vary by boat module. The web app tolerates
variants (`Course Over Ground` / `course_over_ground` / `CourseOverGround` /
`cog` / `COG`; `Sog`/`SOG`/`Speed`; `Beam`/`Width`). Mobile has partial
coverage in `viam_connection.dart` and `ais.dart`. When you touch a reading,
match the web app's full variant set.

---

## Context you'll need

Salvaged from the original scoping doc (`MOBILE_FLUTTER_PLAN.md`, since
retired — this file superseded its plan and `MOBILE_PARITY_GAPS.md` superseded
its port map).

### Architecture

```
┌──────────────────────────── Flutter app ─────────────────────────────┐
│                                                                       │
│  flutter_map                            viam_sdk                      │
│   ├─ TileLayer  ── HTTP ─────────────►  tile/weather server (Go, UNCHANGED)
│   │    /noaa-enc/tile, osm-tile, esri, /noaa-weather/…                │
│   ├─ Marker/Polyline layers  ◄── app state (boat, AIS, waypoints, routes)
│   └─ CustomPainter (wind, isobars, navaid symbols)                    │
│                                                                       │
│  BoatState (ChangeNotifier)  ◄── 1 Hz poll loop                       │
│   ├─ RobotClient (WebRTC)  ─────────►  the boat (movement_sensor,     │
│   │                                     sensors, camera, navigation)  │
│   └─ Viam (app + data)     ─────────►  app.viam.com (config, MQL history)
└───────────────────────────────────────────────────────────────────────┘
```

**The chart is rendered server-side and shipped as XYZ raster tiles.** The
phone renders no ENC. That is the whole reason this port is tractable — and the
reason rule 1 above matters: the Go server is shared, unchanged, by both
clients.

Reuse boundary:
- **Reuse as-is** — the entire Go tile/weather/data server. Mobile is just
  another HTTP client of it.
- **Reuse the protocol, re-implement the client** — the boat poll loop and the
  cloud MQL queries: same component names, same pipelines, written in Dart.
- **Rebuild in Flutter** — all UI.

### Locked product decisions

These were settled before the port started. Don't relitigate them inside a
card; if one needs revisiting, raise it separately.

1. **Both platforms, phone-first.** One adaptive layout with breakpoints, not
   two codebases. Optimise the handheld case; let tablets use the extra room
   (L6).
2. **Auth is full `app.viam.com` login.** OAuth + PKCE, then pick the machine.
   The web app's URL/cookie path is *not* the model. **Already built** —
   `mobile/lib/auth/`, `mobile/lib/screens/`.
3. **Online-only for v1.** Tile disk caching is in scope (L1); region pre-fetch
   and on-device rendering are not (L2, deferred).
4. **Static wind for v1.** Grid-sampled arrows, not animated particles — this
   deliberately keeps the hardest rendering problem off the critical path (F1,
   deferred).

### Server endpoints

Both clients consume these. Read-only; no auth beyond the machine connection.

| Endpoint | Content |
|----------|---------|
| `/noaa-enc/tile/{z}/{x}/{y}.png` | ENC charts rendered to PNG; `style`, `navaids`, `skip`, `sd`, `v` params (A1–A3) |
| `/noaa-enc/osm-tile/{z}/{x}/{y}.png` | OSM land underlay |
| `/noaa-enc/navaids?minLon&minLat&maxLon&maxLat` | GeoJSON aids to navigation (B1) |
| `/noaa-enc/structures?minLon&minLat&maxLon&maxLat` | GeoJSON bridges/cables/pipes (B2) |
| `/noaa-enc/compare/{z}/{x}/{y}.png`, `/compare/test` | render-diff vs NOAA WMS (J8) |
| `/noaa-weather/models` | model catalogue: `minFh`/`maxFh`/`stepFh`/`disabled` (F2) |
| `/noaa-weather/data/{model}/latest.json?fh=N` | wind/wave grids (already used) |
| `/noaa-wms/proxy` | NOAA official WMS passthrough (A4) |
| `/app-config` | `{tileServerBaseURL, chartOnly}` (A6) |
| `/myboat-icon` | operator's own-boat icon (C1) |
| `/version` | hot-reload signal — **web only, not needed on mobile** |

External services the clients call directly: Esri World Imagery (satellite
base), NOAA CO-OPS (tides, G1), open-meteo (local forecast, G2), NASA GIBS and
nowCOAST (satellite/lightning overlays, M7).

### Beta-SDK parity checks

`viam_sdk` is beta ("breaking changes may occur in patch versions"). Confirmed
working in the app today: `RobotClient` over WebRTC, `MovementSensor`,
`Sensor.readings()`, `Camera.getImages()`, `robot.resourceNames`,
`robot.getCloudMetadata()`, `dataClient.tabularDataByMql`.

**Unverified, and cards depend on them** — check before estimating:

- `NavigationClient` waypoint methods + `doCommand` **Struct** requirement → E1, E2
- `appClient.getRobotPart(...).configJson` → H6, H5

If one is missing, say so in the PR and park the card rather than working
around it.

---

## Running this backlog (for the human)

### Where a card can run

The Flutter toolchain **cannot be provisioned in Claude Code web sessions** for
this org: the SessionStart hook clones Flutter, but the Dart SDK download from
`storage.googleapis.com` is refused by egress policy (403). So a web-session
agent can't run `pub get`, `analyze` or `test` — its only feedback is CI, which
takes about a minute for analyze+test. That splits the backlog in two:

- **Remote-safe** — pure-logic cards: a new file, translated tests, no UI to
  eyeball. A green CI run *is* the acceptance. These are also conflict-free.
  → **D3** (cpa), **D4** (mmsi), **E4** (gpx), **G3** (moon), the `simplify`
  half of **E3**, the `route_store` half of **E2**, the `tileFullyInUSWaters`
  helper of **A5**, the scale helpers of **C1**.
- **Local only** — everything with map or UI behaviour, i.e. most cards. Run
  these where there's a toolchain and a device.

(If someone allowlists `storage.googleapis.com` for session egress, this
distinction goes away and the whole backlog can run remotely.)

### Local agents in worktrees

One worktree per card. Several branches stay checked out at once, each directly
`flutter run`-able, so you can flip between candidate builds on the phone
without stashing:

```bash
git worktree add ../cp-A1 -b claude/A1-tile-params origin/main
cd ../cp-A1 && claude          # then: /card A1
```

When it's done and merged:

```bash
git worktree remove ../cp-A1 && git branch -d claude/A1-tile-params
```

`.claude/settings.json` already sets `worktree.bgIsolation: none`, and
`/card <id>` (`.claude/commands/card.md`) carries the per-card instructions, so
the only thing you type per agent is the card ID.

**Run 3–4 concurrently, not a dozen.** The bottleneck is not agent throughput,
it's how many PRs you can load onto a device and actually judge — realistically
2–4 a day.

### Pick cards that don't collide

`map_screen.dart` has been split into `mobile/lib/map/` so parallel work lands
in different files. That halved the worst pile-up (22 cards on one file) but did
not remove it: layer-adding cards inherently converge on the layer stack.
Check a card's `Files:` line before running two agents together. Current hot
spots:

| File | Cards |
|------|-------|
| `map/map_layers.dart` | A1 A5 B1 B2 C2 C4 D2 D10 E1 F3 F4 F8 J7 L4 |
| `map_screen.dart` | E1 F7 J1 J2 J3 J4 J6 L4 L6 |
| `viam_connection.dart` | D1 H1 H4 H6 L3 L5 |
| `map/map_controls.dart` | E5 J1 J2 J3 J4 J5 |
| `data_drawer.dart` | G1 H1 H4 K1 L6 |
| `tile_sources.dart` | A1 A2 A3 A5 L1 |
| `map/wind_overlay.dart` | F2 F3 F7 F8 |

A good parallel batch takes one card from each row — e.g. **A1** (layers) +
**H6** (connection) + **J5** (controls) + **D3** (new file) run cleanly
together. Two cards from the `map_layers.dart` row do not; run those in
sequence, or accept a merge.

### Batch by test session, not by milestone

Some cards simply cannot be judged at the dock. Group them by what one sitting
can verify:

| Wave | Verified how | Cards |
|------|--------------|-------|
| 1 | At the dock, chart-only, web app open beside it | A1 A2 A3 A5 J6 J7 |
| 2 | At the slip, boat connected | D10 H6 K1 C1 L5 |
| 3 | Underway | C2 C4 J1 J2 J4 |

Each card's **Accept** list says what to look for. Agents can't check any of it
on hardware, so their PRs should tick only what they verified (analyze, tests,
logic) and leave the rest for you.

---

## Milestone status

| # | Milestone | Cards | Est. | Status |
|---|-----------|-------|------|--------|
| M0 | Correctness quick-wins | A1 A2 A3 D10 J6 H6 (+README ✅) | 2–3 d | ☐ |
| M1 | Helm fundamentals | A5 A6 C1 C2 C4 J1 J2 J4 J7 K1 L5 | 1.5 wk | ☐ |
| M2 | Safety overlays | B1 B2 B4 D1 D2 D3 G1 | 1.5 wk | ☐ |
| M3 | Routes & waypoints | E1 E2 E3 E4 E5 | 2 wk | ☐ |
| M4 | Marine hardening | C3 L1 L3 L4 L6 | 1.5 wk | ☐ |
| M5 | Weather & environment | F2 F3 F4 F7 F8 G2 G3 G4 | 2 wk | ☐ |
| M6 | Systems, cameras, polish | H1 H2 H4 I1 I2 J3 J5 J8 | 1 wk | ☐ |
| M7 | Long tail | A4 B3 D4–D9 F1 H3 H5 H7 L2 | 2–3 wk | ☐ |

All P0s are done at the end of M3 (~5.5 wk).

---

# M0 — Correctness quick-wins

Small, no new subsystems, high leverage. The chart stops being subtly wrong and
the app stops forgetting everything on launch.

### A1 · Zoom-tiered chart tile params `S` `P0` — ✅ **done** (screenshot
comparison against web still worth an on-water check)

**Files:** `mobile/lib/tile_sources.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:3730-3795`

**Problem.** Mobile pins one URL — `?style=ecdis&landfill=0` — at every zoom.
The web app requests three different renders depending on zoom, because at
higher zooms the *vector* navaid/structure layers take over and the tile must
stop baking those features in. Mobile's fixed URL means the detail chart is the
wrong render, and `landfill=0` is a documented no-op that forks the server's
tile cache into a redundant shard for no benefit.

**What web does** (`tileUrlFunction`, thresholds `VECTOR_TILE_NAVAID_MIN_Z=12`,
`VECTOR_TILE_STRUCTURE_MIN_Z=14`):

| Zoom | Params |
|------|--------|
| `z < 12` (overview) | `style=ecdis` |
| `12 ≤ z < 14` (mid) | `style=wms&navaids=0` |
| `z ≥ 14` (detail) | `style=wms&navaids=0&skip=BRIDGE,CBLOHD,PIPOHD,CONVYR` |

Plus `v=<version>` (A3) and `sd=<feet>` (A2) on all three. Chart tiles are never
requested below z7.

**Do.**
1. Replace the checkmate entry's fixed `urlTemplate` with a per-tile URL
   builder. `flutter_map`'s `TileLayer` takes `urlTemplate` *or* a
   `TileProvider`; the cleanest hook is a custom `TileProvider` subclass whose
   `getTileUrl(coords, options)` picks the param set from `coords.z`.
2. Drop `landfill=0` entirely.
3. Keep `minZoom: 7` — matches the web app's gate and avoids triggering ~10 s
   server-side overview renders for tiles nobody sees.
4. Until B1/B2 land, mobile has no vector navaid/structure layers, so the mid
   and detail tiers would strip features nothing redraws. **Ship A1 with the
   thresholds behind two constants defaulting to "off"** (i.e. always overview)
   and flip them on in B1/B2. Document the coupling in a comment on both sides.

**Accept.**
- [ ] Screenshots at z9 / z13 / z15 match the web app's chart at the same
      position and zoom. *(manual, on-water)*
- [x] No request contains `landfill=`.
- [x] No tile request is issued below z7 (`minZoom: 7` on the checkmate
      TileSource; provider covered by `test/tile_params_test.dart`).

**Watch out.** `flutter_map` `coords.z` is the *tile* z; the web app
deliberately gates on the **view** zoom for the OSM suppression (A5) because OL
rounds fractional view zoom when picking tiles. For the chart params, tile z is
correct — for A5's suppression, it is not. Don't conflate them.

---

### A2 · Safe-depth (`sd`) draft param `S` `P0` — ✅ **done**

**Files:** `mobile/lib/config.dart`, `mobile/lib/tile_sources.dart`
**Web ref:** `src/marineMap.svelte:344-353, 3629`
**Depends on:** A1 (shares the URL builder)

**Problem.** `sd` drives the DEPARE gradient on ENC tiles: solid coral below the
safe depth, gradient to white at 2×. It's the single most safety-relevant chart
parameter and mobile never sends it, so every boat gets the server default
regardless of draft.

**What web does.** Reads `?safeDepth=N` (feet) from the page URL; when absent it
sends nothing and the server falls back to its `safe_depth_ft` config attribute.

**Do.**
1. Add `SAFE_DEPTH_FT` as a `--dart-define` in `Config` (empty = omit the param,
   matching web).
2. Append `sd=<value>` to chart tile URLs when non-empty.
3. Surface it in the UI as a settings field — on mobile there's no URL to edit,
   so a compile-time-only knob is useless to an operator. Persist via the same
   store as J6 and rebuild the tile layer on change (change the `TileLayer` key
   so `flutter_map` drops its cache).

**Accept.**
- [x] Setting safe depth to 6 ft vs 20 ft visibly changes the shading band
      (tile-layer key includes `sd`, so the cache drops and refetches).
- [x] Empty value sends no `sd` param at all (not `sd=`) —
      `test/chart_url_extras_test.dart`.
- [x] The value survives an app restart (persisted via J6's Settings).

---

### A3 · Tile cache-buster `S` `P1` — ✅ **done**

**Files:** `mobile/lib/tile_sources.dart`
**Web ref:** `src/marineMap.svelte:325-339, 3628`
**Depends on:** A1

**Problem.** Web appends `v=<git short hash>` to every tile URL so a new build
busts both OpenLayers' and the HTTP cache. Mobile sends nothing, so a device can
serve stale tiles indefinitely after a server-side render change — and once L1
adds a disk cache, *permanently*.

**Do.** Append `v=` to chart tile URLs, sourced from the app's package version
plus build number (`package_info_plus`), or a `--dart-define` build stamp.
Whatever the source, it must change on every release. Add a manual "clear tile
cache" action alongside it when L1 lands.

**Accept.**
- [x] Every chart tile request carries a `v=` param.
- [x] The value changes between two builds from different commits (version +
      build number via package_info_plus; CI stamps the run number).

---

### D10 · Bound the AIS marker layer `S` `P0` — ✅ **done**

**Files:** `mobile/lib/map/map_layers.dart`, `mobile/lib/boat_state.dart`
**Web ref:** `src/marineMap.svelte:2582-2637` (style fn), `4082` (layer)

**Problem.** `map_screen.dart` builds a `Marker` for **every** AIS target in
`state.aisBoats` on every rebuild, with no viewport cull and no cap — and
`setState` fires on each 1 Hz `BoatState` tick. In a busy harbour (hundreds of
targets) that's hundreds of `Transform.rotate` widgets rebuilt per second.
The wind overlay already learned this lesson: it caches markers and rebuilds
only on event handlers, capped at 1500 (`_rebuildWindMarkers`). AIS didn't.

**Do.**
1. Cull to the visible bounds (plus a small margin so panning isn't jumpy),
   using the `_bounds` the map already tracks in `onPositionChanged`.
2. Cache the built marker list; rebuild only when the AIS set changes or the
   viewport moves — not on every state tick. Mirror `_rebuildWindMarkers`.
3. Cap the count (start at 500) and, when over, prefer targets that are closer,
   faster, or have a CPA (once D3 lands). **Log what was dropped** — a silently
   truncated traffic picture is worse than a visibly capped one.
4. At low zoom, drop the rotation transform (a 3 px triangle's heading is
   invisible anyway) or hide AIS entirely below a zoom gate.

**Accept.**
- [ ] 300 synthetic targets hold 60 fps while panning (profile mode).
      *(manual, on-device profile run)*
- [x] Panning away and back shows the same targets
      (`test/ais_cull_test.dart`).
- [x] The drop count is visible in the debug screen when the cap trips
      ("AIS not drawn" row).

---

### J6 · Persist map and tool state `S` `P0` — ✅ **done**

**Files:** new `mobile/lib/settings.dart`, `mobile/lib/map_screen.dart`
**Web ref:** `src/marineMap.svelte:272-284` (keys), `292-320`, `364-420`

**Problem.** Mobile persists **nothing**: every launch opens at the hardcoded
Long Island Sound centre at z9 with the default base layer. The web app persists
its whole map state — in **cookies**, not localStorage.

**What web persists** (cookie name → meaning, 365-day expiry):

| Cookie | Value |
|--------|-------|
| `mapViewCenter` | `[lon, lat]` JSON; removed when the user re-anchors on the boat |
| `mapViewZoom` | number, validated `0 < z ≤ 22` |
| `mapLayers` | `{layerName: bool}` for every layer |
| `mapHeadsUp` | `"1"`/`"0"` |
| `mapBoatPosition` | `"center"`/`"bottom"` |
| `mapAutoZoom` | `"1"`/`"0"` |
| `mapHeadingLineLengthNm` | one of `1,2,3,5,10,15` (default 5) |
| `mapAisProjectionMin` | one of `1,2,5,10` (default 2) |
| `mapAircraftProjectionMin` | one of `1,2,5` (default 2) |

Every loader validates and falls back to the default on garbage — do the same.

**Do.**
1. Add a small `Settings` wrapper over `shared_preferences` with typed
   get/set + validation, keyed to match the web names where a value has the same
   meaning (makes cross-referencing easy; there's no actual shared storage).
2. Persist on change, restore on launch: view centre/zoom, base layer,
   course-up, and every toggle that exists today (wind on/off and forecast hour
   are reasonable to persist too — the web app doesn't, but the mobile launch
   cost is higher).
3. Restore *before* the first frame so there's no visible jump, and don't let
   the existing "recentre on first GPS fix" (`_followedFirstFix`) stomp a
   restored view the user deliberately left somewhere else — first fix should
   only auto-centre when there is no persisted view.

**Accept.**
- [x] Kill and relaunch: same position, zoom, base layer, orientation.
- [x] Corrupt a stored value by hand → app launches on defaults, no crash
      (`test/settings_test.dart`).

---

### H6 · Honour `chartplotter-hide` `S` `P1`

**Files:** `mobile/lib/viam_connection.dart` (`_discoverCameras` and friends)
**Web ref:** `src/App.svelte:838`, `1305-1373` (`updateMachineConfig`,
`findComponentConfig`)

**Problem.** Operators mark components as hidden from the chartplotter with a
`chartplotter-hide` attribute in the machine config. The web app reads the part
config and skips them. Mobile ignores it — the TODO is already flagged in the
`_discoverCameras` doc comment — so hidden cameras and sensors show up on the
phone.

**Do.**
1. On the login path, fetch the part config via `appClient.getRobotPart(...)`
   and cache it (web caches per fragment; a single fetch on connect is enough
   here — it's read once per session).
2. Build a `Set<String>` of hidden component names and filter every discovery
   helper through it.
3. On the API-key path there's no cloud client — degrade gracefully (no
   filtering) rather than failing discovery.

**Accept.**
- [ ] A camera marked `chartplotter-hide` on the test machine does not appear.
- [ ] The API-key path still connects and discovers with no cloud access.

**Watch out.** `getRobotPart(...).configJson` availability in the Dart SDK is
one of the flagged beta-parity unknowns (see Context above). Verify
first; if it's missing, say so in the PR and park H6 + H5 rather than working
around it.

---

### M0-doc · Fix `mobile/README.md` `S` `P1` — ✅ **done**

The README claimed the app "deliberately does not yet do AIS, weather, routes,
camera, or history." All but routes existed. Rewritten against the actual code,
with the file table extended to the modules that landed after the spike and the
roadmap pointed at this file.

---

# M1 — Helm fundamentals

After M1 a full run out and back is navigable without opening the web app.

### A5 · Under-chart OSM fallback `M` `P0`

**Files:** `mobile/lib/tile_sources.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:101-131` (coverage boxes + tile test),
`3531-3563` (layer + `tileUrlFunction`)

**Problem.** Mobile shows exactly one base layer at a time. NOAA ENC only covers
US waters, so anywhere else the chart base renders **blank**. The web app always
keeps OSM *underneath* the chart and suppresses the OSM fetch only for tiles
that fall entirely inside US ENC coverage — so foreign and uncharted waters
still show land.

**What web does.** `US_ENC_COVERAGE` is 7 generous lon/lat rectangles (CONUS,
Alaska mainland, Aleutians across the dateline, Hawaii, PR/USVI, Guam/CNMI,
American Samoa). `tileFullyInUSWaters(z,x,y)` converts the XYZ tile to a lon/lat
box and tests **full containment** — partially-covered tiles at coverage edges
still load OSM, so there are no blank seams. OSM is skipped only when
`viewZoom >= 7 && chartBaseActive() && tileFullyInUSWaters(...)`.

**Do.**
1. Port `US_ENC_COVERAGE` and `tileFullyInUSWaters` to Dart verbatim, with unit
   tests: a mid-Atlantic tile → false; a Long Island Sound tile → true; a tile
   straddling the coverage edge → false; an Aleutian tile either side of the
   dateline → true.
2. Stack two `TileLayer`s: OSM below, chart above. Restructure `_base` from
   "which single layer" to "which chart base (or none)".
3. Gate the suppression on the **view** zoom (`_map.camera.zoom`), not the tile
   z — the web comment at `:3549` explains why: gating on tile z produced an
   all-white map in the 6.5–7 band.

**Accept.**
- [ ] Pan to the Caribbean or the Med with the chart base selected → land is
      visible from OSM, not a blank screen.
- [ ] Long Island Sound issues no OSM tile requests at z≥7.
- [ ] Zooming slowly through 6.5→7.5 never shows an all-white map.

---

### A6 · Runtime `/app-config` `M` `P1`

**Files:** `mobile/lib/config.dart`, connection bootstrap
**Web ref:** `src/marineMap.svelte:3500-3524`, server side `module.go:374-381`

**Problem.** `TILE_BASE` is a compile-time `--dart-define`, so one build can
only ever talk to one tile server. The web app fetches `/app-config` at runtime:

```json
{"tileServerBaseURL": "https://…", "chartOnly": false}
```

**Do.**
1. After connecting to a machine, fetch `<base>/app-config` and adopt
   `tileServerBaseURL` when non-empty; fall back to the `--dart-define`, then to
   the hosted default already in `Config.tileBase`.
2. Honour `chartOnly`: skip boat polling and show the chart-only UI.
3. Cache the resolved base per machine so a reconnect doesn't re-probe, and
   rebuild tile layers when it changes.

**Accept.**
- [ ] Pointing at a server with a different `tileServerBaseURL` moves tile
      traffic to that host with no rebuild.
- [ ] A server returning `chartOnly: true` yields a working chart and no boat
      polling.
- [ ] An unreachable `/app-config` falls back silently to the default.

---

### C1 · Real boat icon, scaled `S` `P1`

**Files:** `mobile/lib/map/boat_marker.dart`, assets
**Web ref:** `src/marineMap.svelte:155-184` (`dimScaleFactor`, `boatScaleAxes`),
`3025-3055` (`probeMyBoatIcon`), `3056-3101` (`createBoatStyle`)

**Problem.** Mobile draws `Icons.navigation` in red. The web app uses a
top-down boat SVG, scaled by the vessel's real length and beam, and swaps in an
operator-supplied icon from `/myboat-icon` when the module exposes one.

**What web does.** Reference vessel 24.38 m (80 ft) × 6.0 m beam; scale factor
`sqrt(value/reference)` clamped to `[0.6, 2.5]`; length scales the long axis,
beam the cross-axis, and when beam is unknown the cross-axis stays at default
(so a tanker reads long, not also fat). `/myboat-icon` is probed once on mount;
the override is remapped by height ratio against the bundled icon's natural
73 px so it renders at the same on-screen size at any resolution.

**Do.** Bundle the same `topdown-boat.svg`; port `dimScaleFactor`/`boatScaleAxes`
with tests; probe `/myboat-icon` once per session and fall back to the bundle on
any failure. Apply to the own-boat marker only — AIS markers keep their own icon
(see D5).

**Accept.**
- [ ] Own boat renders as a boat, oriented to heading.
- [ ] A machine with `myboat_icon_path` set shows that icon at the same size.
- [ ] An 800 ft vessel is longer but not wider when no beam is reported.

---

### C2 · Live track behind the boat `M` `P0`

**Files:** new `mobile/lib/track.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:2696-2703` (`depthToColor`), `2704-2745`
(style), `2796-2900` (`addTrackFeature`, `recordTrackPoint`), `1463`
(pruning)

**Problem.** Mobile draws no track at all. Seeing where you've just been is
basic chartplotter behaviour — and it's how you spot set and drift.

**What web does.** Records a point per tick when the position moves; each
segment carries the depth and speed at that point; a toggle colours the track by
depth (`depthToColor`: 0 ft red → 10 ft+ green, linear, so shoal water is
obvious); old features are pruned to bound memory.

**Do.**
1. Accumulate track points in `BoatState` (it already keeps timestamped metric
   history — same pattern, plus lat/lng). Add a minimum-distance filter so a
   moored boat doesn't accumulate thousands of identical points.
2. Draw with a `PolylineLayer`. For the depth-coloured mode, emit one polyline
   per segment with its own colour (`flutter_map` has no per-vertex gradient);
   watch the segment count and simplify when it grows — reuse the `simplify.ts`
   port from E3 if that has landed.
3. Add the depth-colour toggle to the layer controls, persisted via J6.
4. Cap retained points and prune oldest-first (this is gap **C5** — web does it
   in `pruneOldTrackFeatures`, `src/marineMap.svelte:1463`; there's no separate
   card because a track without pruning isn't shippable).

**Accept.**
- [ ] A 30-minute run leaves a visible track.
- [ ] Depth colouring makes a shoal transit visibly red.
- [ ] Sitting at a dock for an hour doesn't grow memory unboundedly.

---

### C4 · Heading line `S` `P1`

**Files:** `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:3328-3376`, layer `3961`, length options
`:317`

**Do.** Draw a line from the boat along its heading, length selectable from
`[1, 2, 3, 5, 10, 15]` nm (default 5), persisted via J6. Use proper great-circle
offset, not a flat-earth delta — at 15 nm the error is visible. Toggle it in the
layer controls.

**Accept.**
- [ ] The line points along heading (not COG) and ends at the selected distance
      measured with the ruler (J1).
- [ ] The length choice survives a restart.

---

### J1 · Measure tool `S` `P1`

**Files:** `mobile/lib/map/map_controls.dart`, `mobile/lib/map_screen.dart` (gesture mode)
**Web ref:** `src/marineMap.svelte:3102-3121`, `3381-3429` (`handleMeasureClick`),
`511-523` (`bearingDeg`)

**Do.** A toolbar toggle that puts the map in measure mode: tap sets the anchor,
drag or a second tap sets the end, showing distance (nm) and bearing (°T) live
in a pill. Escape hatch: tapping the toggle again clears. Touch idiom — prefer
tap-tap over click-drag, and put the readout where a thumb isn't covering it.
Reuse the same distance helper as C4/E5 (`getDistance` equivalent — use
`latlong2`'s `Distance`, and mirror the web's `bearingDeg`).

**Accept.**
- [ ] A known 10 nm leg measures 10.0 ± 0.05 nm.
- [ ] Bearing matches the web app's reading for the same two points.

---

### J2 · Boat position on screen `S` `P1`

**Files:** `mobile/lib/map_screen.dart`, `mobile/lib/map/map_controls.dart`
**Web ref:** `src/marineMap.svelte:3318-3327`, applied in `updateFromData:1216`
and `maybeReanchorOnBoat:4229`

**Do.** Toggle between boat-centred and boat at 80 % down the screen (look-ahead
mode — what you want under way). Web computes the target pixel as
`[w/2, h/2]` or `[w/2, h*0.8]` and calls `view.centerOn(...)`. In `flutter_map`,
offset the camera target by the equivalent screen delta. Persist via J6.

**Accept.**
- [ ] In bottom mode the boat sits ~80 % down and stays there while following.
- [ ] Pinch-zoom keeps the boat anchored at its screen position (see the web
      comment at `:1193` about zoom-anchor fighting the recentre logic).

---

### J4 · Follow mode with pan-to-suspend `S` `P0`

**Files:** `mobile/lib/map_screen.dart`, `mobile/lib/map/map_controls.dart`
**Web ref:** `src/marineMap.svelte:3430-3448` (`stopPanning`), `4229-4246`
(`maybeReanchorOnBoat`), `1188-1245` (`updateFromData`)
**Depends on:** J2

**Problem.** Mobile centres once on the first fix and then never again unless
you hit the FAB. The web app *continuously* follows the boat, suspends following
the moment you drag (5 px threshold on `pointerdrag`), shows a **"Stop
Panning"** button while suspended, and resumes on tap.

**Do.**
1. Follow the boat on every position update unless suspended.
2. Suspend on user pan (use the drag gesture, not a centre-diff — the web
   comment at `:1193` documents why a diff-based check false-positives when the
   zoom anchor moves the geographic centre in bottom mode).
3. Show a "Follow" / "Stop panning" affordance; tapping resumes and clears the
   persisted view centre.
4. Never auto-centre on a null-island `[0,0]` fix — web guards this with
   `isValidCoordinate` (`:504`) and mobile must too, or a boat with no GPS yanks
   the chart to the Gulf of Guinea.

**Accept.**
- [ ] Under way with follow on, the boat stays put on screen.
- [ ] One drag suspends following; the resume affordance appears.
- [ ] A boat reporting `[0,0]` never moves the view.

---

### J7 · Scale line `S` `P1`

**Files:** `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte` — OL's `ScaleLine` control

**Do.** Add a scale bar (`flutter_map`'s `Scalebar`), in nautical miles, placed
clear of the existing bottom-left wind slider. Without it there is no way to
judge distance at a glance.

**Accept.**
- [ ] Scale bar reads in nm and updates with zoom.
- [ ] It doesn't collide with the wind slider when wind is on.

---

### K1 · Surface machine/resource health `S` `P1`

**Files:** `mobile/lib/debug_screen.dart`, `mobile/lib/data_drawer.dart`
**Web ref:** `src/App.svelte:1609-1622` (`findComponentStatus`), `machineStatus`
in `globalData`

**Problem.** Mobile's status chip is binary: connected or not. When a single
sensor is failing (a dead depth transducer, a wedged AIS receiver) the app
silently shows a stale or missing value with no explanation.

**Do.** Poll `getMachineStatus()` on the slow cadence, and show per-component
state in the debug screen and next to the affected reading in the drawer
(the `sources` map already tracks which component each reading came from).

**Accept.**
- [ ] A component in error is visibly flagged, not silently blank.

---

### L5 · Device-GPS fallback `S` `P1`

**Files:** `mobile/lib/viam_connection.dart`, `mobile/lib/boat_state.dart`
**Web ref:** none — mobile-only capability

**Problem.** When the boat's GPS is unavailable (no movement sensor, dead
sensor, or the WebRTC connection is down) mobile shows no position at all — even
though the phone in your hand knows where it is. This is a real advantage the
web app can't have.

**Do.** Fall back to the device GPS (`geolocator`) when the boat position is
missing or stale, and **label it unmistakably** — a different marker colour plus
an explicit "phone GPS" indicator. Never silently substitute: a helmsman must
know whether the fix is the boat's. Request permission lazily, at first use, and
degrade cleanly on refusal.

**Accept.**
- [ ] Disconnecting the boat switches to a visibly-distinct phone-GPS marker.
- [ ] Denying location permission leaves the app working with no position.
- [ ] Boat GPS returning takes precedence again automatically.

---

# M2 — Safety overlays

The milestone that makes it a chartplotter rather than a boat dashboard: the
chart's features become tappable, and traffic gets a collision readout.

### B1 · Navaids vector layer `L` `P0`

**Files:** new `mobile/lib/chart/navaids.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:1512-1543` (`s57ColourToCss`), `1545-1623`
(colours + icon src), `1624-1760` (`buoyBody`, `beaconBody`), `1790-1804`
(style), `1805-1959` (`navaidClassLabel`, `lightCharLabel`, `colourLetters`,
`formatNavaidTooltip`), layer `3649-3679`
**Server:** `render/handlers.go:294-347`
**Depends on:** B4 (do B4 first or together)

**Problem.** Mobile's navaids exist only as pixels baked into the tile. You
can't tap a buoy to see what it is, and at detail zooms A1's `navaids=0` render
would remove them entirely — so B1 gates A1's mid/detail tiers.

**Endpoint.**
```
GET /noaa-enc/navaids?minLon=&minLat=&maxLon=&maxLat=
→ GeoJSON FeatureCollection of Point features
  properties: {"class": "BOYLAT"|"BOYCAR"|"BOYISD"|"BOYSAW"|"BOYSPP"|"BOYINB"
                        |"BCNLAT"|"BCNCAR"|"BCNISD"|"BCNSAW"|"BCNSPP"
                        |"LIGHTS"|"DAYMAR", ...S-57 attributes}
  Cache-Control: public, max-age=60
```

**Do.**
1. Port the S-57 decoding: colour codes → RGB, `class` → human label, light
   characteristic codes → labels (`lightCharLabel`), colour CSV → letter codes
   (`colourLetters`). These are pure lookup tables — port them wholesale with
   tests, they are the bulk of the value and the easiest thing to get subtly
   wrong.
2. Draw the symbols. Web synthesises SVG per feature (`buoyBody`, `beaconBody`)
   keyed by class + colours. In Flutter, a `CustomPainter` per symbol class is
   the direct equivalent; cache painted symbols by (class, colours) — there are
   only a few dozen distinct combinations and they repeat constantly.
3. Load per viewport (B4), turn the layer on at **z ≥ 12**, and flip A1's
   `VECTOR_TILE_NAVAID_MIN_Z` on in the same PR so the tile stops baking them.
4. Tap → bottom sheet with the formatted metadata (`formatNavaidTooltip` is the
   content spec; hover has no touch equivalent, so it's a sheet, and the tap
   target must be ≥44 pt even though the symbol is smaller).

**Accept.**
- [ ] Every navaid visible on the web chart at z13 is present and tappable.
- [ ] A lit buoy's sheet shows its light characteristic and colours matching
      the web tooltip for the same feature.
- [ ] Panning a harbour for 10 minutes doesn't grow memory without bound (B4).

**Watch out.** This is the largest single-card port in the backlog and the one
most likely to overrun. If it does, split: symbols + tap sheet first, the
long tail of rare classes second.

---

### B2 · Structures vector layer `M` `P1`

**Files:** `mobile/lib/chart/structures.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:1960-1990` (`bridgeCategoryLabel`),
`1991-2036` (labels + icons), `2037-2097` (style), `2098-2127` (tooltip),
layer `3688-3716`
**Server:** `render/handlers.go:350+`
**Depends on:** B1 (same machinery), B4

**Do.** Same pattern as B1 against `/noaa-enc/structures` — bridges, overhead
cables, overhead pipes, conveyors. The tooltip content is the point: **vertical
clearance** is what you need before passing under something. Turn the layer on
at **z ≥ 14** and flip A1's `VECTOR_TILE_STRUCTURE_MIN_Z` plus the
`skip=BRIDGE,CBLOHD,PIPOHD,CONVYR` param in the same PR.

**Accept.**
- [ ] A bridge's sheet shows its clearance and category, matching web.
- [ ] At z14+ each structure is exactly one icon (no doubled tile+vector draw).

---

### B4 · Viewport loading + feature cap `S` `P1`

**Files:** new `mobile/lib/chart/bbox_source.dart`
**Web ref:** `src/marineMap.svelte:3640-3660` (`capVectorSource`), OL's
`bboxStrategy`

**Problem.** `flutter_map` has no equivalent of OpenLayers' `bboxStrategy` +
feature eviction, and B1/B2 are unusable without it: naive per-pan refetching
hammers the server, and naive accumulation grows without bound over a long
coastal session.

**Do.** A reusable GeoJSON-over-bbox source that: tracks which extents have
already been loaded and skips refetching them; debounces on pan/zoom settle
rather than firing per frame; evicts everything and reloads the current viewport
when the retained feature count crosses a cap (**web uses 3000 per layer**);
and coalesces in-flight requests. Unit-test the extent bookkeeping — that's
where the bugs are.

**Accept.**
- [ ] Panning back over already-loaded water issues no new request.
- [ ] Crossing the cap drops features and the current viewport repopulates.
- [ ] Rapid panning issues a bounded number of requests, not one per frame.

---

### D3 · CPA / TCPA `S` `P0`

**Files:** new `mobile/lib/cpa.dart`, `mobile/lib/map/ais_sheet.dart`
**Web ref:** `src/marineMap.svelte:5147-5180` (`computeCpa`), rendered in the
popup at the `CPA` label

**Problem.** The single most safety-relevant AIS readout, and mobile doesn't
have it. Closest point of approach and time-to-CPA are how you decide whether a
crossing target is a problem.

**What web does.** Flat-earth projection around own position
(`mPerDegLat = 111132.92 - 559.82*cos(2*lat)`,
`mPerDegLng = 111412.84*cos(lat)`), relative velocity from both COGs and SOGs,
`tcpa = -(d·dv)/|dv|²`, CPA = distance at that time. Returns null when either
COG is missing or relative motion is below `1e-6`. The UI only shows it when
`tcpaMin >= 0` (a target already past its CPA isn't a threat).

**Do.** Port `computeCpa` verbatim with unit tests — head-on closing, parallel
same-course (no CPA), crossing, and already-diverging. Show CPA/TCPA in the AIS
bottom sheet, and use it to prioritise which targets survive D10's cap.

**Accept.**
- [ ] Two synthetic targets on a known crossing produce the same CPA/TCPA as
      the web app to 2 decimal places.
- [ ] Missing COG → no CPA row rather than a wrong one.

---

### D1 · AIS history tracks `M` `P1`

**Files:** `mobile/lib/viam_connection.dart`, `mobile/lib/ais.dart`
**Web ref:** `src/App.svelte:565-578` (`aisSamplesToPoints`), `579-601`
(`fetchAisHistory`), `602-627` (`fetchAisBoatHistory`), `628-637`
(`onBoatPopupOpen`)

**What web does.** Two DoCommands on the AIS sensor:

```dart
// all targets, polled every 60 s, only when the ais-track layer is on
sensor.doCommand({'command': 'all_history'})
// → { "<mmsi>": [ {Location: [lat,lng], Created|Timestamp: <ts>}, … ], … }

// one target, fired immediately when its popup opens
sensor.doCommand({'command': 'history', 'mmsi': <int mmsi>})
```

Samples without a valid 2-element `Location` are skipped; timestamp is
`Created` falling back to `Timestamp`, epoch-0 if absent.

**Do.** Port both. Poll `all_history` at 60 s **only while the track layer is
on** (web gates on `aisTracksNeeded`), and fetch a single vessel's history when
its sheet opens so the track appears immediately rather than up to 60 s later.
Render as polylines under the AIS markers, subject to D10's viewport cull.

**Accept.**
- [ ] Opening a target's sheet shows its track without waiting for the poll.
- [ ] Turning the track layer off stops the 60 s poll entirely.

---

### D2 · AIS projection vectors `S` `P1`

**Files:** `mobile/lib/map/map_layers.dart`
**Web ref:** `src/marineMap.svelte:377-391` (persisted minutes), `4101-4111`
(virtual layer), drawn inline by `aisStyleFunction`

**Do.** Draw a line ahead of each target showing where it will be in N minutes
along its COG at its SOG. N selectable from `[1, 2, 5, 10]` (default 2),
persisted via J6. Do the same for own boat. This plus D3 is how you read a
traffic situation at a glance.

**Accept.**
- [ ] A 10 kn target's 6-minute vector measures 1.0 nm with the ruler (J1).
- [ ] Targets with no COG draw no vector.

---

### G1 · Tides `M` `P0`

**Files:** new `mobile/lib/tides.dart`, `mobile/lib/data_drawer.dart`
**Web ref:** `src/marineMap.svelte:5281-5313` (station list + cache),
`5315-5340` (`nearestTideStation`), `5341-5386` (`fmtNoaaDate`,
`fetchTidePredictions`), `5387-5465` (clip/interp/synth), `5466-5550`
(`refreshTide`, formatting)

**Problem.** No tide information at all. On the US East Coast that's the
difference between a channel and a mud flat.

**Endpoints** (NOAA, called directly — not through the Go module):
```
GET https://api.tidesandcurrents.noaa.gov/mdapi/prod/webapi/stations.json
      ?type=tidepredictions
    → {stations: [{id, name, lat, lng}, …]}   (cache it — it's large and static)

GET https://api.tidesandcurrents.noaa.gov/api/prod/datagetter
      ?product=predictions&interval=hilo|h&begin_date=YYYYMMDD&range=72
      &application=viam-chartplotter&station=<id>&datum=MLLW
      &time_zone=lst_ldt&units=english&format=json
```
Web fetches both `hilo` (next high/low) and `h` (hourly series for the
sparkline) over a 72 h window starting *yesterday*, so there are points either
side of now.

**Do.**
1. Fetch and cache the station list to disk (web caches in `sessionStorage`;
   on mobile it should survive a launch — it's static reference data).
2. Find the nearest station to the boat (haversine, `R = 3440.065` nm), refresh
   when the boat has moved enough to change the answer.
3. Show current level (interpolated between hourly points), next high and next
   low with times, and a sparkline. Port the clip/interp/synth helpers with
   tests — `synthSeriesFromHilo` is the fallback when the hourly product is
   unavailable for a station.
4. Link out to the station's NOAA page.

**Accept.**
- [ ] Next high/low times match the NOAA station page for the same day.
- [ ] Offline (or NOAA down) degrades to "tides unavailable", not a crash.
- [ ] The station list isn't refetched on every launch.

---

# M3 — Routes & waypoints

The largest missing subsystem and the last P0. Do E2's data layer first — E1's
UI depends on the same nav-service plumbing.

### E2 · Saved routes data layer + sheet `L` `P1`

**Files:** new `mobile/lib/routes/route_store.dart` (+ tests), routes UI
**Web ref:** `src/lib/routeStore.ts` (+ `routeStore.test.ts` — **port the
tests**), `src/lib/RoutesPanel.svelte`
**Backend (read-only reference):** `nav.go:441-490`, `nav_routes.go`,
`ROUTES_SPEC.md`

**Contract.** All storage goes through the nav service's DoCommand verbs — the
client never touches the cloud directly, so this works without app credentials:

```dart
nav.doCommand({'routes_list': true})          // → {routes: [Route, …]}
nav.doCommand({'routes_save': {'route': {…}}})
nav.doCommand({'routes_delete': {'id': id}})
nav.doCommand({'routes_rename': {'id': id, 'name'?: …, 'notes'?: …,
                                 'color'?: …, 'updatedAt'?: …}})
```

`Route` = `{id, name, notes?, color?, source: "manual"|"track", createdAt,
updatedAt, waypoints: [{lat,lng}], stats?: {distanceNm, count}, scope?:
"location"|"parent"}`. **`scope: "parent"` routes are inherited from an
ancestor location and are read-only** — the UI must not offer save/rename/delete
on them. Read-modify-write, schema and size guards all live on the Go side;
the client just shapes the calls and warns above **200 KiB** (`SIZE_WARN_BYTES`)
before the backend rejects.

**Do.**
1. Port `routeStore.ts` to Dart including `newRouteId` and the 8-colour palette,
   with `routeStore.test.ts`'s cases translated.
2. Build the routes sheet: list, load, load-reversed, rename, delete, colour
   pick, and preview-on-map (including "show all"). Preview polylines are
   display-only and must be visually distinct from the active route.
3. Loading a route uses `set_waypoints` (see E1) — one atomic replace.

**Accept.**
- [ ] A route saved on the phone appears in the web app's routes panel and
      vice versa.
- [ ] A `parent`-scoped route shows as read-only with no destructive actions.
- [ ] Saving past the size warning shows the warning before the backend rejects.

---

### E1 · Waypoint editing `L` `P0`

**Files:** `mobile/lib/routes/`, `mobile/lib/map/map_layers.dart` (rendering), `mobile/lib/map_screen.dart` (gestures)
**Web ref:** `src/App.svelte:2241-2331` (CRUD), `2332-2348` (`loadNavRoute`),
`src/marineMap.svelte:3122-3162` (add/clear), `3163-3262` (feature sync +
drag-modify), `3263-3311` (insert/delete popups)

**Problem.** Mobile can only *view* the active destination. You can't plan or
adjust a route from the phone at all.

**Nav service API** (`VIAM.NavigationClient` → Dart `NavigationClient`):

| Operation | Call |
|-----------|------|
| list | `getWayPoints()` → `[{id, location{latitude,longitude}}]` |
| add | `addWayPoint(latitude:, longitude:)` |
| remove | `removeWayPoint(id)` |
| insert | `doCommand({'insert_waypoint': {'before_id': id, 'lat': , 'lng': }})` |
| move | `doCommand({'move_waypoint': {'id': , 'lat': , 'lng': }})` |
| replace all | `doCommand({'set_waypoints': {'waypoints': [{'lat','lng'}, …]}})` |
| clear | loop `removeWayPoint` over existing ids |

**Do.**
1. Poll `getWayPoints()` and render the waypoint chain + active route line.
2. Editing, touch-first: **long-press on the chart** to add a waypoint at that
   point; **drag a waypoint** to move (commit on release, not per frame);
   **tap a waypoint** for a sheet with delete / insert-before; a **clear route**
   action with a confirm step (web arms a two-tap confirm — `clearConfirmArmed`).
3. Apply optimistically with `pending-<ts>` ids, exactly as web does, and
   reconcile from the next poll — the backend ids are what subsequent
   move/insert/remove need, so never send a pending id.
4. After a `set_waypoints`, refetch so local ids are real backend ObjectIDs.

**Accept.**
- [ ] Add / move / insert / delete / clear all round-trip and survive a poll.
- [ ] A route edited on the phone shows the same waypoints in the web app.
- [ ] Dragging a waypoint issues one `move_waypoint`, not one per frame.
- [ ] Clearing requires deliberate confirmation.

**Watch out.** `NavigationClient.doCommand` needs a protobuf `Struct`, not a
plain map — the web app has a scar here (`App.svelte:2329`: a plain object
serialises to an *empty* command and the backend reports "DoCommand
unimplemented"). Check the Dart SDK's equivalent requirement before assuming a
`Map` works.

---

### E5 · Full route stats `S` `P1`

**Files:** `mobile/lib/boat_state.dart`, `mobile/lib/map/map_controls.dart`
**Web ref:** `src/marineMap.svelte:556-612` (`distanceAlongLine`), `537-555`
(`formatDurationMin`, `formatEta`), panel at the `Next` / `Final` labels
**Depends on:** E1

**Do.** Mobile shows next-waypoint distance and ETA. Add **final** destination
distance and ETA across the whole remaining route (web shows it whenever the
route has more than one waypoint), using distance-along-line from the boat's
projected position, and current SOG for the estimate.

**Accept.**
- [ ] On a 3-waypoint route, `Final` distance equals the sum of remaining legs.
- [ ] Both ETAs blank out (not zero) when stationary.

---

### E3 · Save-from-track `M` `P1`

**Files:** `mobile/lib/routes/`, new `mobile/lib/simplify.dart` (+ tests)
**Web ref:** `src/lib/simplify.ts` + `simplify.test.ts` (**port both**),
`src/App.svelte:1548-1608` (`fetchTrackWindow`),
`src/lib/RoutesPanel.svelte:570-640`
**Depends on:** E2, C3 (shares the track fetch)

**Do.** Pick a time window, pull the recorded track for it, simplify
(Douglas-Peucker — `simplifyTrack`), preview the result on the chart, and save
it as a route with `source: "track"`. The window fetch is C3's MQL query with
explicit `[t0, t1]` bounds and the same hot→cold fallback rule (**hot only if
the window's newest edge is within ~2 days**, else straight to cold).

**Accept.**
- [ ] A recorded run becomes a route with a sane waypoint count (tens, not
      thousands).
- [ ] The preview matches what gets saved.
- [ ] `simplify.test.ts`'s cases pass in Dart.

---

### E4 · GPX export `S` `P2`

**Files:** new `mobile/lib/gpx.dart` (+ tests)
**Web ref:** `src/lib/gpx.ts` + `gpx.test.ts`
**Depends on:** E2

**Do.** Port the GPX writer and hand the file to the platform share sheet
(`share_plus`) — that's the mobile equivalent of the web download, and how you
get a route onto a Garmin. Import is **not** in scope here; if you want it,
raise it as a new card (the web app has no importer either).

**Accept.**
- [ ] Exported file opens in a GPX reader with the right waypoints.
- [ ] `gpx.test.ts`'s cases pass in Dart.

---

# M4 — Marine hardening

No web counterpart: the browser runs at the dock, the phone goes to sea. This
milestone is what makes the app survive a real run.

### L1 · Tile disk cache `M` `P0`

**Files:** `mobile/lib/tile_sources.dart`, `mobile/pubspec.yaml`
**Depends on:** A3 (cache-busting must exist before caching is safe)

**Problem.** Every pan refetches tiles over the boat's connection. Server-side
chart renders are expensive and cellular offshore is slow and metered.

**Do.** Add a disk-backed tile store (`flutter_map_cache` or
`flutter_map_tile_caching` — evaluate both, prefer the lighter one that doesn't
drag in a database if a simple store suffices). Bound it by size with LRU
eviction, key on the full URL **including `v=` and `sd=`** so a version bump or
draft change doesn't serve stale pixels, expose usage plus a "clear cache"
action in settings, and serve from cache when offline instead of showing blank.

**Accept.**
- [ ] Re-panning previously-seen water issues no network requests.
- [ ] Airplane mode still renders cached water.
- [ ] Cache size stays under the configured bound.
- [ ] Changing safe depth (A2) doesn't serve the old shading.

---

### L3 · Adaptive polling & background behaviour `M` `P0`

**Files:** `mobile/lib/viam_connection.dart`, `mobile/lib/main.dart`

**Problem.** The app polls at a flat 1 Hz forever, plus camera frames at 1 Hz
while the camera screen is open, plus tiles. That's a shore-Wi-Fi budget being
spent on a cellular or satellite link, and a battery being drained in a pocket.

**Do.**
1. ~~**Pause polling when backgrounded.**~~ Done: `main.dart` cancels the poll
   timer on `paused`/`hidden` via `ViamConnection.pause()`, and `resume()`
   restarts it plus settles the connection immediately (skipping the probe
   outright when the app was away long enough that the peer is certainly gone).
2. **Adapt the rate.** Full 1 Hz when the map is visible and the boat is moving;
   back off hard when stationary or when only the drawer is open. The existing
   `_tickN % 5` staggering for AIS/route/systems is the right pattern to extend.
3. **Budget cellular.** Track bytes for tiles + polls; surface it in settings;
   offer a "low data" mode that lengthens intervals and leans on the L1 cache.
4. Keep the connection watchdog behaviour intact — every boat RPC in
   `viam_connection.dart` is deadline-bounded and the heartbeat outcome is
   recorded the moment it's known (`_noteHeartbeat`), so a slower poll cadence
   must not slow down or mask death detection.

**Accept.**
- [x] Backgrounded for 10 minutes: no network traffic, and resume reconnects.
- [ ] Stationary at anchor uses measurably less data than under way.
- [ ] A measured baseline for "1 hour under way" exists in the PR description.

---

### L4 · Night mode + keep-awake helm mode `M` `P1`

**Files:** theming, `mobile/lib/map_screen.dart`, `mobile/lib/map/map_layers.dart`

**Problem.** A white-ish chart at night destroys night vision, and the screen
sleeping at the helm makes the app useless exactly when you need it.

**Do.** A night mode that dims the chart (a red-tinted overlay above the tile
layer is the cheap, effective approach — the server has no night render) and
switches the UI to red-on-black; keep the screen awake (`wakelock_plus`) while
the map is foregrounded, as an explicit toggle rather than always-on. Both
persisted via J6. Consider an auto-switch at sunset using G2's sun times.

**Accept.**
- [ ] Night mode leaves the chart readable without a bright white field.
- [ ] Screen stays awake with the toggle on, sleeps normally with it off.

---

### L6 · Phone vs tablet layout `M` `P1`

**Files:** `mobile/lib/map_screen.dart`, `mobile/lib/data_drawer.dart`
**Reference:** the locked product decision — both platforms, phone-first,
one adaptive layout (not two codebases)

**Do.** Use `LayoutBuilder` breakpoints. Phone keeps today's design: full-screen
chart, data in a drawer. Tablet promotes the data panel to a persistent side
panel (closer to the web app's helm dashboard) and spreads the map controls out.
One widget tree, breakpoint-driven — no forked screens.

**Accept.**
- [ ] Tablet in landscape shows chart + persistent data panel without a drawer.
- [ ] Phone is unchanged.
- [ ] Rotating a tablet doesn't lose state.

---

### C3 · Recorded track from the cloud `M` `P1`

**Files:** `mobile/lib/history.dart`, `mobile/lib/track.dart`
**Web ref:** `src/App.svelte:1408-1422` (`bucketIdConcat`), `1428-1452`
(`buildTabularQuery`), `1453-1491` (`runTabularQuery`), `1521-1547`
(`positionHistoryMQL` + alternates), `1548-1608` (`fetchTrackWindow`),
`1768-1786` (`positionHistoryMQLNamed`)
**Depends on:** C2

**Problem.** `HistoryService` backfills scalar metrics but there's no position
history, so the track only exists from app launch. The web app shows ~7 days.

**Pipeline shape** (mirrors the existing `HistoryService.fetch`, but
`method_name: "Position"` and reading `data.coordinate.{latitude,longitude}`):

```
$match:  {location_id, robot_id, component_name: <leaf>,
          method_name: "Position", time_received: {$gte: t0, $lte: t1}}
$sort:   {time_received: -1}
$group:  {_id: <minute bucket>, ts: {$min: "$time_received"},
          pos: {$first: "$data"}}
$sort:   {ts: 1}
```

Web buckets by a concatenated `yy-M-d H:m` string (`bucketIdConcat`); the
existing Dart `HistoryService` buckets by epoch division — either is fine, keep
one style. Hot store first, cold on empty; for an explicit window, go straight
to cold when the window's newest edge is older than ~2 days.

**Do.** Add a position-history fetch to `HistoryService`, try the configured
movement sensor then each alternate name (H7 generalises this), and seed the
track on connect. Feed E3's window fetch from the same code path.

**Accept.**
- [ ] Opening the app shows the last day's track before any live points.
- [ ] A boat whose GPS component was renamed still resolves history.
- [ ] No visible seam where recorded history meets live points.

---

# M5 — Weather & environment

### F2 · Weather model picker `M` `P1`

**Files:** `mobile/lib/weather.dart`, `mobile/lib/map/wind_overlay.dart`
**Web ref:** `src/lib/WeatherOverlays.svelte:1020-1082`
**Server:** `weather/noaa_weather_cache.go:126,135-139`,
`weather/weather_models.go:961-977`

**Problem.** Mobile hardcodes `'gfs'` and a fixed 0–240 h / 3 h slider. The
server publishes a model catalogue with per-model forecast ranges.

**Endpoint.**
```
GET /noaa-weather/models
→ [{name, displayName, kind, domain, minFh, maxFh, stepFh, disabled, reason}, …]
```

**Do.** Fetch the catalogue, offer the enabled models, and drive the forecast
slider's range and step from the selected model's `minFh`/`maxFh`/`stepFh`
rather than the hardcoded 0/240/3. Show `reason` on disabled entries (web puts
it in the option's tooltip; on mobile use a subtitle). Persist the choice via
J6.

**Accept.**
- [ ] Switching to a short-range model reshapes the slider to its range.
- [ ] A disabled model is visibly unavailable with its reason.
- [ ] `/noaa-weather/models` unreachable → fall back to GFS defaults.

---

### F3 · Wave overlay `M` `P1`

**Files:** `mobile/lib/weather.dart`, `mobile/lib/map/wind_overlay.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/lib/WeatherOverlays.svelte:483-494`, `src/lib/windLayer.ts`
(`WAVE_COLOR_SCALE`, `colorForValue`), legend at `:958`
**Depends on:** F2

**Do.** Same JSON grid path as wind, magnitude-only. Render as a coloured grid
or contour band with the web's `WAVE_COLOR_SCALE`, plus a legend in feet
(`WAVE_RANGE_MAX_M = 3`, `METERS_TO_FEET = 3.28084`). Port the colour scale and
`colorForValue` so both apps colour the same sea state identically.

**Accept.**
- [ ] Wave colours match the web app for the same forecast hour and area.
- [ ] Legend reads in feet.

---

### F8 · Weather overlay zoom gates `S` `P1`

**Files:** `mobile/lib/map/map_layers.dart`, `mobile/lib/map/wind_overlay.dart`
**Web ref:** `LayerOption.maxZoom` (`src/marineMap.svelte:70-79`)

**Do.** Hide the 0.25° wind/wave fields past the zoom where one model cell spans
hundreds of screen pixels and the field becomes a meaningless wash. Web gates
via each layer's `maxZoom`. Show a short "zoom out for weather" hint rather than
silently vanishing.

**Accept.**
- [ ] Zooming to chart detail hides the field and explains why.

---

### G2 · Local forecast `M` `P1`

**Files:** new `mobile/lib/forecast.dart`, drawer UI
**Web ref:** `src/lib/WeatherOverlays.svelte:743-800`

**Endpoint** (open-meteo, called directly):
```
GET https://api.open-meteo.com/v1/forecast
  ?latitude=&longitude=
  &current=temperature_2m,wind_speed_10m,wind_direction_10m
  &hourly=temperature_2m,precipitation,wind_speed_10m
  &daily=sunrise,sunset
  &temperature_unit=fahrenheit&wind_speed_unit=kn&precipitation_unit=inch
  &timezone=auto&forecast_hours=4&forecast_days=2
```

**Do.** Show air temp, wind speed/direction, 4 h rain total, and sunrise/sunset
in the drawer. Web picks *tomorrow's* sunrise/sunset once today's sunset has
passed, so the panel stays useful at night — do the same. Refresh on a slow
timer and when the boat moves materially; cache the last response so a dropout
shows stale-but-labelled data rather than blanks.

**Accept.**
- [ ] Values match the web app's panel at the same position.
- [ ] After sunset, the sun row shows tomorrow's times.
- [ ] Offline shows the last values marked stale, not an error.

---

### G3 · Moonrise / moonset `S` `P2`

**Files:** new `mobile/lib/moon.dart` (+ tests)
**Web ref:** `src/lib/moon.ts` + `moon.test.ts` (SunCalc port, BSD-2-Clause)
**Depends on:** G2 (shares the panel row)

**Do.** Port the SunCalc-derived calculation with its tests — it's pure maths,
no API (open-meteo has no moon data). Keep the upstream attribution comment.

**Accept.**
- [ ] `moon.test.ts`'s cases pass in Dart.
- [ ] Times match the web app for the same date and position.

---

### G4 · Windy deep link `S` `P2`

**Files:** drawer UI
**Web ref:** `src/marineMap.svelte:6460-6470`

**Do.** Open windy.com at the boat's position via `url_launcher`, for the deep
forecast the app doesn't render itself.

---

### F4 · Isobars `M` `P2`

**Files:** `mobile/lib/weather.dart`, `mobile/lib/map/map_layers.dart`
**Web ref:** `src/lib/isobarLayer.ts` (284 lines), layer at
`WeatherOverlays.svelte:495-505`
**Depends on:** F2

**Do.** Port the contour generation to Dart and draw the polylines with pressure
labels. The algorithm is self-contained; the risk is label placement, which is
fiddly on a small screen — label sparsely.

**Accept.**
- [ ] Isobar geometry matches the web app for the same forecast hour.

---

### F7 · Point weather sample `S` `P2`

**Files:** `mobile/lib/map/wind_overlay.dart`, `mobile/lib/map_screen.dart` (long-press)
**Web ref:** the `cursorInfo` readout (`src/marineMap.svelte`, `Cursor` label)
**Depends on:** F3

**Do.** The web app samples wind/wave under the mouse cursor. There is no
cursor on a phone: make it **long-press → a callout** with wind speed/direction
and wave height at that point, plus range and bearing from the boat. Reuse
`WindField.sampleInterp`, which already exists in `weather.dart`.

**Accept.**
- [ ] Long-press shows wind and wave at that point, and dismisses on tap-away.
- [ ] Doesn't fight E1's long-press-to-add-waypoint — they must be in different
      modes, and the interaction spec must say which wins.

---

# M6 — Systems, cameras, panel polish

### H1 · Gauge grouping, gallons, level colours `S` `P1`

**Files:** `mobile/lib/data_drawer.dart`, `mobile/lib/viam_connection.dart`
**Web ref:** `src/App.svelte:2145-2187` (`organizeGauges`), `src/helpers.ts`
(`tankSort`), tank markup at `App.svelte` gauges block

**Do.** Mobile shows bare percentages. Add: absolute volume in gallons where the
sensor reports `Capacity`, the web's level colour thresholds (so a low tank
reads red at a glance), and the web's gauge grouping. `tankSort` is **already
ported** in `viam_connection.dart` — reuse it, don't re-derive it.

**Accept.**
- [ ] A tank with capacity shows both percent and gallons.
- [ ] Low tanks are visibly flagged, matching the web thresholds.

---

### H2 · Sparkline touch detail + range toggle `S` `P2`

**Files:** `mobile/lib/sparkline.dart`, `mobile/lib/graph_screen.dart`
**Web ref:** `src/App.svelte:1979-1990` (`toggleShortGraphRange`), `2219-2240`
(`gaugeHistoricalTsByKey`, `formatAgo`), hover markup in the tank block

**Do.** Web hovers a sparkline for value + "how long ago". On touch that becomes
**drag-to-scrub** on the detail graph with a value/time callout. Add the
short/long range equivalent — the graph screen already has 15 m / 1 h / 4 h /
all windows, so mostly this is surfacing freshness ("3 min ago") next to the
value.

**Accept.**
- [ ] Scrubbing the graph shows value and timestamp under the finger.
- [ ] Each metric shows how stale its newest sample is.

---

### H4 · Seakeeper control `S` `P1`

**Files:** `mobile/lib/data_drawer.dart`, `mobile/lib/viam_connection.dart`
**Web ref:** `src/App.svelte:2349-2368` (`seakeeper`)

**Problem.** Mobile is read-only. Web can toggle Seakeeper power and stabilise.
This and waypoints are the only **write** actions in either app.

**Do.** Add power / stabilise toggles issuing the same DoCommand the web app
sends (read `seakeeper()` for the exact command shape — don't guess). Treat it
as an action on physical equipment: confirm before switching, show in-flight
state, and reflect the *sensor-reported* state afterwards rather than assuming
the command took.

**Accept.**
- [ ] Toggling changes the boat's actual state and the UI reflects the readback.
- [ ] A failed command surfaces an error and does not show a false state.

---

### I1 · Camera tap-to-enlarge `S` `P1`

**Files:** `mobile/lib/camera_screen.dart`
**Web ref:** `src/App.svelte:2369-2400` (`enlargeImage`, Esc to close)

**Do.** Tap a camera tile → full-screen with pinch-zoom and swipe between
cameras; back gesture closes. While enlarged, poll only the visible camera —
the current screen polls every camera every second regardless, which is the
bandwidth-heaviest thing the app does.

**Accept.**
- [ ] Enlarged view zooms and pans.
- [ ] Only the visible camera is fetched while enlarged.

---

### I2 · Camera staleness and failure handling `S` `P2`

**Files:** `mobile/lib/camera_screen.dart`
**Web ref:** `src/App.svelte:877-894` (`removeCamera`), `lastCameraTimes`

**Do.** Show each frame's age, and drop a camera that fails repeatedly instead
of retrying it forever at 1 Hz. Mirror the fuel screen's freshness treatment —
that pattern is already right in this codebase.

---

### J3 · Auto-zoom by speed `S` `P2`

**Files:** `mobile/lib/map_screen.dart`, `mobile/lib/map/map_controls.dart`
**Web ref:** `src/marineMap.svelte:3450-3453`, formula in `updateFromData:1218`
**Depends on:** J4

**Do.** Optional, default **off**. Web's formula:
`zoom = floor(16 - pow(floor(speedKn), 0.41)) + zoomModifier`, clamped to ≥1
(z10 ≈ 30 miles, z16 ≈ city). Any manual pan or zoom force-disables it — see
`stopPanning`, which clears the flag deliberately so the formula stops fighting
the user's chosen zoom.

**Accept.**
- [ ] Speeding up zooms out; a manual zoom turns the feature off.

---

### J5 · Scalable layers panel `M` `P1`

**Files:** `mobile/lib/map/map_controls.dart`
**Web ref:** `src/marineMap.svelte:186-236` (grouping, auto-hide), panel markup

**Problem.** Today's `DropdownButton` lists 4 base layers. After M2/M5 there
are chart bases *plus* navaids, structures, tracks, AIS + sub-toggles, wind,
waves, isobars, heading line, areas — a dropdown can't express that.

**Do.** A layers sheet with the web app's structure: radio-exclusive base
layers, then independent overlays, with parent/child grouping (AIS → track,
projection) where a disabled parent greys its children. Wire every toggle to J6.
Keep it thumb-reachable and scrollable — the web app's own panel had to be made
scrollable for the same reason (#48).

**Accept.**
- [ ] Every layer is toggleable from one sheet, with children under parents.
- [ ] Turning a parent off disables its children.
- [ ] All states persist.

---

### J8 · Tile URL debug `S` `P2`

**Files:** `mobile/lib/debug_screen.dart`
**Web ref:** `src/marineMap.svelte:3460-3499` (`showTileUrlForClick`)

**Do.** A debug-mode tap that copies the tile URL for the tapped point at the
current zoom, plus the `/noaa-enc/compare/{z}/{x}/{y}.png` and
`/noaa-enc/compare/test?lat=&lon=` links. Cheap, and it's how chart render bugs
get reported with something actionable.

---

# M7 — Long tail

Lower value per unit effort, or explicitly deferred. Pull forward only on
demand. Each still has a full gap entry in `MOBILE_PARITY_GAPS.md`.

| # | Task | Effort | Notes |
|---|------|--------|-------|
| F1 | **Animated wind particles** | L | The known-hardest item, deferred since the original plan. `CustomPainter` particle pool advected by the field, or a `FragmentShader`. Static arrows already cover most of the value — do this for polish, and watch the battery cost. |
| F6 | Satellite cloud imagery | M | `WeatherOverlays.svelte:310-340,518` — NASA GIBS colour/visible/infrared variants as an image overlay. |
| F5 | Lightning | S | `WeatherOverlays.svelte:506-517` — nowCOAST strike overlay. |
| B3 | Areas from `area` components | M | `App.svelte:1072-1147` (discovery + `areaVisibleToday`), `marineMap.svelte:893-951,1761-1789,4176`. Folder grouping and date-range visibility included. |
| D6 | `ais-web-sender` + `airstream` sources | L | `App.svelte:728-780,1049-1071`. Airstream needs viewport-bbox DoCommand with debounce; mind the cellular cost of a global AIS feed. |
| D7 | Boats panel (fleet) | M | `marineMap.svelte:474-503,613-660`. Search, per-boat visibility, select/deselect all, fit-all-visible. Needs a real touch design — it's a dense desktop panel today. |
| D8 | ADS-B aircraft | M | `App.svelte:532-546`, `marineMap.svelte:2145-2440`. Sparse fields: render whatever keys are present, don't assume a fixed set. Projection options `[1,2,5]` min. |
| D9 | Detections overlay | M | `marineMap.svelte:2652-2695`, `src/lib/BoatInfo.ts`. |
| D4 | MMSI country flags | S | Port `src/lib/mmsi.ts` (~250 MID entries) wholesale. |
| D5 | Length-scaled AIS icons | S | `marineMap.svelte:2545-2581`; shares C1's scaling helpers. |
| A4 | OpenSeaMap / NOAA WMS / ECDIS bases | M | `marineMap.svelte:3603,3835,3859`. WMS needs the `/noaa-wms/proxy` path when reachable. |
| H3 | AC/Victron detail + yacht page | M | `App.svelte:389-404,781-807,1023-1030`, `YachtDetails.svelte`. Per-line volts/amps, Victron powers, door sensors. |
| H5 | Remote-part data scoping | M | `App.svelte:1623-1767`. Needs `getRobotPart().configJson` — same beta-SDK dependency as H6; verify before starting. |
| H7 | Movement-sensor alternates | S | `App.svelte:1192,1529-1547`. Falls out of C3 nearly for free. |
| L2 | Offline region pre-fetch | L | "Download this area before leaving the dock." Explicitly out of v1 scope; the biggest *product* gap for real offshore use. Depends on L1. |

---

## Cross-cutting work

Not milestones — fold each into the card that first needs it.

### X1 · Port pure logic with its tests

These are dependency-free and already unit-tested on the web side. Porting the
tests alongside is how the two clients stay honest.

| Web module | Tests | Ported by | Target |
|-----------|-------|-----------|--------|
| `src/lib/simplify.ts` | `simplify.test.ts` | E3 | `mobile/lib/simplify.dart` |
| `src/lib/gpx.ts` | `gpx.test.ts` | E4 | `mobile/lib/gpx.dart` |
| `src/lib/moon.ts` | `moon.test.ts` | G3 | `mobile/lib/moon.dart` |
| `src/lib/routeStore.ts` | `routeStore.test.ts` | E2 | `mobile/lib/routes/route_store.dart` |
| `src/lib/mmsi.ts` | — | D4 | `mobile/lib/mmsi.dart` |
| `computeCpa` (`marineMap.svelte:5147`) | — | D3 | `mobile/lib/cpa.dart` |
| `tileFullyInUSWaters` (`marineMap.svelte:121`) | — | A5 | with tile sources |
| `dimScaleFactor`/`boatScaleAxes` (`:155`) | — | C1 | with the boat marker |

Gap **L7** (test coverage: 2 mobile test files vs the web's four suites plus the
Go tests) is closed by doing this — every ported module lands with its tests, so
coverage grows with the backlog rather than as a separate push. CI already runs
`flutter test`; keep it green.

### X2 · One reading-parse layer per side

Sensor key spellings are duplicated across `viam_connection.dart` and
`ais.dart`, and already cover fewer variants than the web app. Centralise
parsing into one module with table-driven tests, and cover the full variant set
(`Course Over Ground`/`course_over_ground`/`CourseOverGround`/`cog`/`COG`;
`Sog`/`SOG`/`Speed`; `Beam`/`Width`; `Created`/`Timestamp`). Do this the first
time a card touches reading parsing.

### X-none · Gaps with no card, by decision

- **K2** (discovery breadth — `adsb`, `airstream`, web senders, Victron, doors,
  `area` components, nav service) has no card of its own: each is discovered by
  the card for the feature that consumes it. Nothing is lost, but don't read
  K2's absence as "done".
- **K3** (`/version` hot-reload) is deliberately **not** ported. It exists so a
  browser picks up a new module build without a manual refresh; an app store
  build has no equivalent need.

### X3 · Keep this file current

Tick acceptance boxes in the PR that lands a card; flip the milestone table row
when its last card lands. The gap IDs are the shared vocabulary between this
file and `MOBILE_PARITY_GAPS.md` — don't renumber them.

---

## Risks

- **B1/B2 vector volume.** OpenLayers gives bbox loading and feature eviction
  for free; `flutter_map` gives neither. B4 is the mitigation and it is on the
  critical path for M2 — if B4 slips, B1 and B2 slip with it. Test in a busy
  harbour, not open water.
- **E1 touch design.** Drag-to-move and click-to-insert are mouse idioms. The
  touch equivalents (long-press, drag handles, undo) are design work, not a
  port. Budget for a design pass, and expect to iterate on the water.
- **`viam_sdk` beta drift.** E1/E2 need `NavigationClient` DoCommand;
  H5/H6 need `getRobotPart().configJson`. Both are flagged unknowns. **Verify
  before committing to an estimate** — if a symbol is missing, say so in the PR
  and park the card rather than working around it.
- **Battery and data (L3).** Measure a real hour under way before offshore
  testing, not after. The app currently has no idea what it costs to run.
- **Cards that grow.** B1, E1 and E2 are each near the top of the "one session"
  size. If one is overrunning, split it and say so — a half-finished vector
  layer that ships is worth more than a complete one that doesn't.
