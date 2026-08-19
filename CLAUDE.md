# viam-chartplotter

A marine chartplotter: a Go Viam module that renders NOAA ENC charts and serves
weather, plus two clients that consume it — a Svelte web app and a Flutter
mobile app.

```
module.go, render/, weather/, nav*.go   Go module: chart tiles, weather, routes,
                                        navigation service. Shared by BOTH clients.
src/                                    Web app (Svelte 5 runes + OpenLayers + TS)
mobile/                                 Mobile app (Flutter + flutter_map + viam_sdk)
```

## Commands

```
make mobile-analyze     # flutter analyze          (CI gate)
make mobile-test        # flutter test             (CI gate)
make mobile-run         # run on a device; add VIAM_* vars for a boat
npm run dev             # web app
go test ./...           # module
```

CI: `.github/workflows/mobile-flutter.yml` runs analyze + test + a debug APK
build on any change under `mobile/`. It builds with no credentials, so the app
must always compile and run chart-only.

## Mobile work

The backlog is **[`mobile-todo.md`](mobile-todo.md)** — task cards (`A1`, `E2`,
…) with web references, steps and acceptance criteria. The analysis behind it is
[`MOBILE_PARITY_GAPS.md`](MOBILE_PARITY_GAPS.md). **Do one card per branch.**
Use `/card <id>` to start one.

### Ground rules

1. **Don't change the Go server.** Both clients depend on it; a change to suit
   mobile can silently break the web app. If a task seems to need one, stop and
   say so.
2. **The web app is the contract for data, not for interaction.** Match its
   units, sensor-key spellings, endpoint params and fallbacks exactly. Do *not*
   port its mouse idioms — hover and right-click have no touch equivalent; each
   card names the touch design.
3. **Port pure logic with its tests.** Anything in `src/lib/` with a `.test.ts`
   beside it gets ported to Dart with the test cases translated. That is how the
   two clients stay in sync.
4. **SI on the wire, display units in the UI.** Sensors report metres, °C, m/s;
   the UI shows feet, °F, knots. Conversions live in one place per side.
5. **Don't block the poll loop.** New network work gets its own cadence, not a
   new call inside `ViamConnection._tick()`.

### Mobile layout

`mobile/lib/map_screen.dart` is composition and state only. The pieces live in
`mobile/lib/map/` — `map_layers.dart` (the FlutterMap child stack),
`map_controls.dart` (floating buttons/chips), `wind_overlay.dart`,
`ais_sheet.dart`, `boat_marker.dart` — so that concurrent tasks land in
different files. Keep it that way: a new chart layer goes in `map_layers.dart`,
a new button in `map_controls.dart`.

### Sensor readings

Key spellings vary by boat module. Tolerate the same variants the web app does:
`Course Over Ground` / `course_over_ground` / `CourseOverGround` / `cog` / `COG`;
`Sog` / `SOG` / `Speed`; `Beam` / `Width`; `Created` / `Timestamp`.

### Beta SDK

`viam_sdk` is beta. Confirmed working: `RobotClient` over WebRTC,
`MovementSensor`, `Sensor.readings()`, `Camera.getImages()`,
`robot.resourceNames`, `getCloudMetadata()`, `dataClient.tabularDataByMql`.
Unverified and blocking specific cards: `NavigationClient` waypoint methods and
its `doCommand` Struct requirement (E1, E2), `getRobotPart().configJson`
(H5, H6). If a symbol is missing, report it — don't work around it.

## Notes

- In Claude Code **web** sessions the Flutter toolchain currently cannot be
  provisioned: the SessionStart hook clones Flutter, but the Dart SDK download
  from `storage.googleapis.com` is blocked by egress policy (403). Analyze and
  test therefore only run locally or in CI. Prefer local sessions for Dart work.
