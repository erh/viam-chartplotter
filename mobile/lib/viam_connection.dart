import 'dart:async';

import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'boat_state.dart';
import 'config.dart';

/// Owns the boat connection and the 1 Hz poll loop. This is the Dart
/// re-implementation of the web app's `doUpdate` loop (src/App.svelte),
/// trimmed to the spike's risk-proving subset: position, speed, heading,
/// and (optionally) depth.
///
/// Two entry points:
///  - [startWithRobot]   — an already-connected RobotClient handed over by the
///                         logged-in session (viam.getRobotClient). The v1 path.
///  - [startWithApiKey]  — direct dial via RobotClient.atAddress + API key, the
///                         original spike path, kept for credential-free runs.
///
/// NOTE (beta SDK): the viam_sdk symbols used here are the documented ~0.3
/// surface; re-verify when the dependency resolves (plan §4.4).
class ViamConnection {
  ViamConnection(this.state);

  final BoatState state;
  RobotClient? _robot;
  String? _movementSensorName;
  String? _depthSensorName;
  Timer? _timer;
  bool _polling = false;
  bool _ownsRobot = false; // close it on dispose only if we dialed it

  Future<void> startWithRobot(RobotClient robot) async {
    _robot = robot;
    _ownsRobot = false; // owned by the session/picker that connected it
    await _afterConnect();
  }

  Future<void> startWithApiKey() async {
    if (!Config.hasBoat) {
      state.setStatus('No boat configured — chart-only mode');
      return;
    }
    state.setStatus('Connecting…');
    try {
      _robot = await RobotClient.atAddress(
        Config.host,
        RobotClientOptions.withApiKey(Config.apiKeyId, Config.apiKey),
      );
      _ownsRobot = true;
    } catch (e) {
      state.setStatus('Connect failed: $e');
      return;
    }
    await _afterConnect();
  }

  Future<void> _afterConnect() async {
    final robot = _robot;
    if (robot == null) return;
    _movementSensorName = await _discoverMovementSensor(robot);
    _depthSensorName = _discoverSensorByName(robot, 'depth', Config.depthSensor);
    state.setStatus(_movementSensorName == null
        ? 'Connected, but no movement_sensor found'
        : 'Connected ($_movementSensorName)');
    _timer = Timer.periodic(const Duration(seconds: 1), (_) => _tick());
  }

  /// Find a sensor component whose name contains [needle] (case-insensitive),
  /// mirroring the web app's name-regex discovery (e.g. /depth/). An explicit
  /// [override] (from --dart-define) wins.
  String? _discoverSensorByName(
      RobotClient robot, String needle, String override) {
    if (override.isNotEmpty) return override;
    try {
      final lower = needle.toLowerCase();
      for (final rn in robot.resourceNames) {
        if (rn.subtype == 'sensor' && rn.name.toLowerCase().contains(lower)) {
          return rn.name;
        }
      }
    } catch (_) {}
    return null;
  }

  /// Mirror of setupMovementSensor: pick the first component whose subtype is
  /// movement_sensor, unless an explicit name was provided via --dart-define.
  Future<String?> _discoverMovementSensor(RobotClient robot) async {
    if (Config.movementSensor.isNotEmpty) return Config.movementSensor;
    try {
      // resourceNames is a synchronous getter (not a Future) in viam_sdk.
      final names = robot.resourceNames;
      for (final rn in names) {
        if (rn.subtype == 'movement_sensor') return rn.name;
      }
    } catch (_) {
      // resourceNames shape can vary across SDK versions; fall through.
    }
    return null;
  }

  Future<void> _tick() async {
    final robot = _robot;
    final msName = _movementSensorName;
    if (robot == null || msName == null || _polling) return;
    _polling = true;
    try {
      final ms = MovementSensor.fromRobot(robot, msName);

      try {
        final p = await ms.position();
        // Position exposes `coordinates` (a GeoPoint), not `coordinate`.
        state.update(
          position: LatLng(p.coordinates.latitude, p.coordinates.longitude),
        );
      } catch (_) {}

      try {
        final v = await ms.linearVelocity();
        state.update(speedKn: v.y * 1.94384);
      } catch (_) {}

      try {
        final h = await ms.compassHeading();
        state.update(headingDeg: h);
      } catch (_) {}

      final depthName = _depthSensorName;
      if (depthName != null) {
        try {
          final r = await Sensor.fromRobot(robot, depthName).readings();
          // Sensor reports Depth in metres (matches the web app); → feet.
          final d = r['Depth'];
          if (d is num) state.update(depthFt: d.toDouble() * 3.28084);
        } catch (_) {}
      }
    } finally {
      _polling = false;
    }
  }

  Future<void> dispose() async {
    _timer?.cancel();
    if (_ownsRobot) await _robot?.close();
  }
}
