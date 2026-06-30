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
/// NOTE (beta SDK): symbol names below — RobotClient.atAddress,
/// RobotClientOptions.withApiKey, MovementSensor.fromRobot/.position()/
/// .linearVelocity()/.compassHeading(), Sensor.fromRobot/.readings(),
/// robot.resourceNames — are the documented surface of viam_sdk ~0.3.
/// Re-verify when the dependency is resolved; they're the most likely
/// thing to drift in a patch release.
class ViamConnection {
  ViamConnection(this.state);

  final BoatState state;
  RobotClient? _robot;
  String? _movementSensorName;
  Timer? _timer;
  bool _polling = false;

  Future<void> start() async {
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
      _movementSensorName = await _discoverMovementSensor(_robot!);
      if (_movementSensorName == null) {
        state.setStatus('Connected, but no movement_sensor found');
      } else {
        state.setStatus('Connected ($_movementSensorName)');
      }
    } catch (e) {
      state.setStatus('Connect failed: $e');
      return;
    }
    _timer = Timer.periodic(const Duration(seconds: 1), (_) => _tick());
  }

  /// Mirror of setupMovementSensor: pick the first component whose subtype is
  /// movement_sensor, unless an explicit name was provided via --dart-define.
  Future<String?> _discoverMovementSensor(RobotClient robot) async {
    if (Config.movementSensor.isNotEmpty) return Config.movementSensor;
    try {
      final names = await robot.resourceNames;
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

      // Position
      try {
        final p = await ms.position();
        state.update(
          position: LatLng(p.coordinate.latitude, p.coordinate.longitude),
        );
      } catch (_) {}

      // Speed over ground (m/s on y → knots), matching the web app.
      try {
        final v = await ms.linearVelocity();
        state.update(speedKn: v.y * 1.94384);
      } catch (_) {}

      // Compass heading (degrees).
      try {
        final h = await ms.compassHeading();
        state.update(headingDeg: h);
      } catch (_) {}

      // Optional depth via a generic sensor (m → ft), like the web loop.
      if (Config.depthSensor.isNotEmpty) {
        try {
          final s = Sensor.fromRobot(robot, Config.depthSensor);
          final r = await s.readings();
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
    await _robot?.close();
  }
}
