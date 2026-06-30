# Viam Chartplotter — Mobile (Phase 0 spike)

A native iOS/Android chartplotter built on the [Viam Flutter SDK](https://flutter.viam.dev/),
per [`../MOBILE_FLUTTER_PLAN.md`](../MOBILE_FLUTTER_PLAN.md). **This is the
Phase 0 spike**, not the product. Its only job is to de-risk the two riskiest
integrations in one runnable app:

1. **`viam_sdk` connects to a real boat over WebRTC** and polls live readings.
2. **`flutter_map` renders the existing server's chart tiles** — proving we
   reuse the Go tile/weather server unchanged and render nothing on the phone.

## What it does

- Connects to a robot via `RobotClient.atAddress` + API key.
- Auto-discovers the first `movement_sensor` and polls position / SOG / heading
  at 1 Hz (the Dart port of the web app's `doUpdate` loop), plus optional depth.
- Shows a full-screen map with a switchable base layer (NOAA ENC / OSM /
  satellite), a boat marker rotated to heading, and a live data panel.
- Recenters on the first GPS fix; a FAB recenters on demand.

It deliberately does **not** do AIS, weather, routes, camera, history, or the
real login flow — those are Phases 1–3.

## Run it

No Flutter toolchain is bundled here. On a machine with Flutter ≥ 3.22:

```bash
cd mobile
flutter create .          # materialize android/ ios/ platform folders (gitignored)
flutter pub get
flutter run \
  --dart-define=VIAM_HOST=boat-main.<loc>.viam.cloud \
  --dart-define=VIAM_API_KEY_ID=<id> \
  --dart-define=VIAM_API_KEY=<key> \
  --dart-define=TILE_BASE=https://<your-tile-server> \
  --dart-define=DEPTH_SENSOR=depth        # optional
```

With no `VIAM_*` defines it starts in chart-only mode (map works, no boat) so
you can verify tile rendering before wiring up credentials. `TILE_BASE`
defaults to the public hosted server.

## Things to verify during the spike (beta-SDK parity, plan §4.4)

The `viam_sdk` symbols used in `lib/viam_connection.dart` are the documented
~0.3 surface but the SDK is beta — confirm against the resolved version:

- `RobotClient.atAddress`, `RobotClientOptions.withApiKey`
- `MovementSensor.fromRobot` → `.position()`, `.linearVelocity()`,
  `.compassHeading()`
- `Sensor.fromRobot` → `.readings()`
- `robot.resourceNames` shape (used for movement-sensor discovery)

Also worth confirming on real hardware: WebRTC connect time and reconnect
behavior on flaky cellular, and tile fetch latency at chart zoom levels.

## Auth note (v1, not the spike)

The product decision is **full app.viam.com login** (plan §6). This spike uses a
static API key for speed. v1 swaps in an OAuth flow whose access token is fed to
`Viam.withAccessToken(token)`; the boat connection then comes from
`viam.getRobotClient(robot)` after the user picks a machine via `appClient`,
rather than the hard-coded `RobotClient.atAddress` used here.

## Files

| File | Role |
|------|------|
| `lib/config.dart` | `--dart-define` config (host, creds, tile base) |
| `lib/viam_connection.dart` | connect + 1 Hz poll loop (port of `doUpdate`) |
| `lib/boat_state.dart` | observable reading snapshot |
| `lib/tile_sources.dart` | base-layer XYZ URLs into the Go server |
| `lib/map_screen.dart` | `flutter_map` UI: layers, boat marker, data panel |
| `lib/main.dart` | app entry |
