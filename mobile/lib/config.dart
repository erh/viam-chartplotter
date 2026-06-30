/// Spike configuration, supplied at build/run time via --dart-define so we
/// don't commit credentials. v1 will replace the API-key fields with an
/// app.viam.com OAuth login that yields an access token (see README §Auth).
///
/// Example:
///   flutter run \
///     --dart-define=VIAM_HOST=boat-main.xxxx.viam.cloud \
///     --dart-define=VIAM_API_KEY_ID=<id> \
///     --dart-define=VIAM_API_KEY=<key> \
///     --dart-define=TILE_BASE=https://<your-tile-server> \
///     --dart-define=DEPTH_SENSOR=depth
class Config {
  /// Robot address, e.g. "boat-main.<loc>.viam.cloud".
  static const String host = String.fromEnvironment('VIAM_HOST');
  static const String apiKeyId = String.fromEnvironment('VIAM_API_KEY_ID');
  static const String apiKey = String.fromEnvironment('VIAM_API_KEY');

  /// Base URL of the existing Go tile/weather server (the chartplotter
  /// module). Tiles are fetched as {z}/{x}/{y}.png from here — no rendering
  /// happens on the phone. Defaults to the public hosted server so the map
  /// works even before a boat is wired up.
  static const String tileBase = String.fromEnvironment(
    'TILE_BASE',
    defaultValue: 'https://chartplotter.viam.cloud',
  );

  /// Optional: a depth sensor name to demonstrate the SensorClient path.
  /// Empty = skip depth.
  static const String depthSensor = String.fromEnvironment('DEPTH_SENSOR');

  /// Optional override for the movement-sensor component name. Empty =
  /// auto-discover the first movement_sensor on the robot.
  static const String movementSensor =
      String.fromEnvironment('MOVEMENT_SENSOR');

  static bool get hasBoat =>
      host.isNotEmpty && apiKeyId.isNotEmpty && apiKey.isNotEmpty;
}
