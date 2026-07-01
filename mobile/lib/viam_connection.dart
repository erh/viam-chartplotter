import 'dart:async';

import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'ais.dart';
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

  /// The live robot connection, for features that fetch on demand (cameras).
  RobotClient? get robot => _robot;
  String? _movementSensorName;
  String? _depthSensorName;
  String? _windSensorName;
  String? _seatempSensorName;
  String? _aisSensorName;
  String? _routeSensorName;
  String? _spotZeroFwName;
  String? _spotZeroSwName;
  String? _seakeeperName;
  List<String> _tankNames = const [];
  List<String> _acPowerNames = const [];
  Timer? _timer;
  int _tickN = 0;
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
    _windSensorName = _discoverSensorByName(robot, 'wind', '');
    _seatempSensorName = _discoverSensorByName(robot, 'seatemp', '');
    _aisSensorName = _discoverSensorEndingWith(robot, 'ais');
    _routeSensorName = _discoverSensorEndingWith(robot, 'route');
    _spotZeroFwName = _discoverSensorByName(robot, 'spotzero-fw', '');
    _spotZeroSwName = _discoverSensorByName(robot, 'spotzero-sw', '');
    _seakeeperName = _discoverSensorByName(robot, 'seakeeper', '');
    _tankNames = _discoverSensorsWhere(
        robot, (n) => n.contains('fuel') || n.contains('freshwater'));
    _acPowerNames =
        _discoverSensorsWhere(robot, (n) => RegExp(r'ac-\d-\d$').hasMatch(n));
    final cameras = _discoverCameras(robot);
    state.setCameras(cameras);
    state.setSources({
      'Movement': _movementSensorName,
      'Depth': _depthSensorName,
      'Wind': _windSensorName,
      'Sea temp': _seatempSensorName,
      'AIS': _aisSensorName,
      'Route': _routeSensorName,
      'Cameras': cameras.isEmpty ? null : cameras.length.toString(),
    });
    state.setStatus(
        _movementSensorName == null ? 'Connected — no GPS' : 'Connected');
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

  /// All sensor components whose (lower-cased) name matches [pred], sorted.
  List<String> _discoverSensorsWhere(
      RobotClient robot, bool Function(String) pred) {
    final out = <String>[];
    try {
      for (final rn in robot.resourceNames) {
        if (rn.subtype == 'sensor' && pred(rn.name.toLowerCase())) {
          out.add(rn.name);
        }
      }
    } catch (_) {}
    out.sort();
    return out;
  }

  bool _boolish(dynamic v) => v == true || (v is num && v != 0);

  /// Camera components on the robot, sorted, with the web app's filtered-camera
  /// rule: drop "<name>" when a "<name>-filtered" sibling exists (prefer the
  /// filtered feed). (chartplotter-hide needs the machine config; not yet done.)
  List<String> _discoverCameras(RobotClient robot) {
    final all = <String>[];
    try {
      for (final rn in robot.resourceNames) {
        if (rn.subtype == 'camera') all.add(rn.name);
      }
    } catch (_) {}
    final names = all.toSet();
    final out = all.where((n) => !names.contains('$n-filtered')).toList()
      ..sort();
    return out;
  }

  /// Like [_discoverSensorByName] but suffix-matched — used for the `ais`
  /// sensor (web regex /\bais$/) so we don't accidentally match unrelated
  /// names that merely contain the substring.
  String? _discoverSensorEndingWith(RobotClient robot, String suffix) {
    try {
      final s = suffix.toLowerCase();
      for (final rn in robot.resourceNames) {
        if (rn.subtype == 'sensor' && rn.name.toLowerCase().endsWith(s)) {
          return rn.name;
        }
      }
    } catch (_) {}
    return null;
  }

  /// Pick the first movement_sensor, unless an explicit name was provided via
  /// --dart-define.
  Future<String?> _discoverMovementSensor(RobotClient robot) async {
    if (Config.movementSensor.isNotEmpty) return Config.movementSensor;
    try {
      for (final rn in robot.resourceNames) {
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
    _tickN++;
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

      // Course over ground lives in the movement sensor's generic readings
      // under one of several key spellings (matches the web app).
      try {
        final rd = await ms.readings();
        final cog = rd['Course Over Ground'] ??
            rd['course_over_ground'] ??
            rd['CourseOverGround'] ??
            rd['cog'] ??
            rd['COG'];
        if (cog is num) state.update(cogDeg: cog.toDouble());
      } catch (_) {}

      final windName = _windSensorName;
      if (windName != null) {
        try {
          final r = await Sensor.fromRobot(robot, windName).readings();
          // Only the true (ground-referenced) wind, like the web app.
          if (r['Reference'] == 'True (ground referenced to North)') {
            final wa = r['Wind Angle'];
            final ws = r['Wind Speed']; // m/s → knots
            state.update(
              windAngleDeg: wa is num ? wa.toDouble() : null,
              windSpeedKn: ws is num ? ws.toDouble() * 1.94384 : null,
            );
          }
        } catch (_) {}
      }

      final tempName = _seatempSensorName;
      if (tempName != null) {
        try {
          final r = await Sensor.fromRobot(robot, tempName).readings();
          final t = r['Temperature']; // °C → °F
          if (t is num) state.update(seaTempF: 32 + t.toDouble() * 1.8);
        } catch (_) {}
      }

      final depthName = _depthSensorName;
      if (depthName != null) {
        try {
          final r = await Sensor.fromRobot(robot, depthName).readings();
          // Sensor reports Depth in metres (matches the web app); → feet.
          final d = r['Depth'];
          if (d is num) state.update(depthFt: d.toDouble() * 3.28084);
        } catch (_) {}
      }

      // AIS targets move continuously but the feed is heavy, so poll it at
      // ~5 s rather than every 1 s tick (matches the web app's slower cadence).
      final aisName = _aisSensorName;
      if (aisName != null && _tickN % 5 == 0) {
        try {
          final r = await Sensor.fromRobot(robot, aisName).readings();
          state.setAis(parseAisReadings(r));
        } catch (_) {}
      }

      // Active route destination from the `route` sensor (rarely changes, so
      // poll on the slow cadence). Cleared when there's no active route.
      final routeName = _routeSensorName;
      if (routeName != null && _tickN % 5 == 0) {
        try {
          final r = await Sensor.fromRobot(robot, routeName).readings();
          final lat = r['destinationLatitude'];
          final lon = r['destinationLongitude'];
          state.setDestination((lat is num && lon is num)
              ? LatLng(lat.toDouble(), lon.toDouble())
              : null);
        } catch (_) {}
      }

      // Boat systems (watermaker / seakeeper / tanks / AC power) change slowly;
      // poll on the slow cadence, offset from AIS+route.
      if (_tickN % 5 == 2) await _pollSystems(robot);
    } finally {
      _polling = false;
    }
  }

  /// Poll the boat-systems sensors (all generic Sensor readings), mirroring the
  /// web app's data panel: watermaker flow, seakeeper, tank levels, AC power.
  Future<void> _pollSystems(RobotClient robot) async {
    final fwName = _spotZeroFwName;
    if (fwName != null) {
      try {
        final r = await Sensor.fromRobot(robot, fwName).readings();
        final f = r['Product Water Flow'];
        if (f is num) {
          state.setSystems(spotZeroFwGph: f.toDouble() * 0.00440287);
        }
      } catch (_) {}
    }
    final swName = _spotZeroSwName;
    if (swName != null) {
      try {
        final r = await Sensor.fromRobot(robot, swName).readings();
        final f = r['Product Water Flow'];
        if (f is num) {
          state.setSystems(spotZeroSwGph: f.toDouble() * 0.00440287);
        }
      } catch (_) {}
    }
    final skName = _seakeeperName;
    if (skName != null) {
      try {
        final r = await Sensor.fromRobot(robot, skName).readings();
        final prog = r['progress_bar_percentage'];
        state.setSystems(
          seakeeperStabilizing: _boolish(r['stabilize_enabled']),
          seakeeperPower: _boolish(r['power_enabled']),
          seakeeperProgress: prog is num ? prog.toDouble() : null,
        );
      } catch (_) {}
    }
    if (_tankNames.isNotEmpty) {
      final tanks = <({String name, double level})>[];
      for (final tn in _tankNames) {
        try {
          final r = await Sensor.fromRobot(robot, tn).readings();
          final lvl = r['Level'];
          if (lvl is num) {
            tanks.add((name: tn.split(':').last, level: lvl.toDouble()));
          }
        } catch (_) {}
      }
      if (tanks.isNotEmpty) state.setSystems(tanks: tanks);
    }
    if (_acPowerNames.isNotEmpty) {
      double vSum = 0;
      int vN = 0;
      double wSum = 0;
      for (final an in _acPowerNames) {
        try {
          final r = await Sensor.fromRobot(robot, an).readings();
          final v = r['Line-Neutral AC RMS Voltage'];
          final a = r['AC RMS Current'];
          if (v is num) {
            vSum += v.toDouble();
            vN++;
            if (a is num) wSum += v.toDouble() * a.toDouble();
          }
        } catch (_) {}
      }
      if (vN > 0) state.setSystems(acVolts: vSum / vN, acWatts: wSum);
    }
  }

  Future<void> dispose() async {
    _timer?.cancel();
    if (_ownsRobot) await _robot?.close();
  }
}
