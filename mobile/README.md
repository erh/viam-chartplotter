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

It now also implements the **app.viam.com login** (the decided v1 auth model):
OAuth + PKCE, token persistence/refresh, and a machine picker — see
[OAuth setup](#oauth-setup-login-path).

It deliberately does **not** yet do AIS, weather, routes, camera, or history —
those are Phases 1–3.

## Run it

No Flutter toolchain is bundled here. On a machine with Flutter ≥ 3.22:

```bash
cd mobile
flutter create .          # materialize android/ ios/ platform folders (gitignored)
flutter pub get
```

There are **two ways to run**, picked automatically by which dart-defines you
pass:

### A) app.viam.com login (the v1 auth model)

Supply the OAuth client registered with Viam and the app routes through
**Sign in with Viam → pick org/location/machine → map**:

```bash
flutter run \
  --dart-define=VIAM_OAUTH_ISSUER=https://<viam-oidc-issuer> \
  --dart-define=VIAM_OAUTH_CLIENT_ID=<client-id> \
  --dart-define=VIAM_OAUTH_REDIRECT=com.viam.chartplotter:/oauthredirect \
  --dart-define=TILE_BASE=https://<your-tile-server>
```

### B) API-key fallback (the original spike path)

Omit the `VIAM_OAUTH_*` defines and the app skips login, dialing a single boat
directly — handy for credential-only testing and CI:

```bash
flutter run \
  --dart-define=VIAM_HOST=boat-main.<loc>.viam.cloud \
  --dart-define=VIAM_API_KEY_ID=<id> --dart-define=VIAM_API_KEY=<key> \
  --dart-define=TILE_BASE=https://<your-tile-server> \
  --dart-define=DEPTH_SENSOR=depth        # optional
```

With no boat defines at all it starts chart-only (map works, no boat) so you can
verify tile rendering first. `TILE_BASE` defaults to the public hosted server.

## OAuth setup (login path)

The Viam Flutter SDK has **no built-in login UI** — it only consumes an access
token via `Viam.withAccessToken`. So login is a standard OAuth 2.0
authorization-code + PKCE flow (`flutter_appauth`) against Viam's identity
provider, with tokens kept in the platform keystore (`flutter_secure_storage`).
See `lib/auth/`.

Two things are required and are **external dependencies**, not code:

1. **Register an OAuth client with Viam** to get a `clientId` and to whitelist
   the redirect URI. The issuer/clientId are environment-specific; pass them via
   `--dart-define` (`lib/auth/oauth_config.dart`). They are not secrets (PKCE
   means no client secret), so dart-define is appropriate.
2. **Native redirect config** (added after `flutter create .`):
   - **Android** — set the AppAuth redirect scheme in `android/app/build.gradle`:
     ```gradle
     android { defaultConfig { manifestPlaceholders += [
       'appAuthRedirectScheme': 'com.viam.chartplotter'] } }
     ```
   - **iOS** — add a URL type with scheme `com.viam.chartplotter` to
     `ios/Runner/Info.plist`.

   The scheme must match `VIAM_OAUTH_REDIRECT`.

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

## CI

`.github/workflows/mobile-flutter.yml` runs `flutter analyze` + a debug-APK
build on changes under `mobile/` (these compiler-check the Dart and surface any
beta `viam_sdk` symbol drift or dependency-resolution issues), with `dart format`
advisory. It builds without any credentials — the app compiles and runs
chart-only when no dart-defines are set — so no secrets are needed.

## Files

| File | Role |
|------|------|
| `lib/auth/oauth_config.dart` | OAuth client config from `--dart-define` |
| `lib/auth/viam_session.dart` | login, token persist/refresh, builds the `Viam` handle |
| `lib/screens/login_screen.dart` | "Sign in with Viam" landing |
| `lib/screens/machine_picker_screen.dart` | org → location → machine picker, opens `RobotClient` |
| `lib/config.dart` | `--dart-define` config (tile base, API-key fallback, sensors) |
| `lib/viam_connection.dart` | connect + 1 Hz poll loop (port of `doUpdate`); takes a session robot or API key |
| `lib/boat_state.dart` | observable reading snapshot |
| `lib/tile_sources.dart` | base-layer XYZ URLs into the Go server |
| `lib/map_screen.dart` | `flutter_map` UI: layers, boat marker, data panel |
| `lib/main.dart` | app entry + login/picker/map routing |
