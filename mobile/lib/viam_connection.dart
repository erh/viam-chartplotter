import 'dart:async';
import 'dart:convert';

import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'ais.dart';
import 'boat_state.dart';
import 'config.dart';
import 'history.dart';

/// Component names marked `chartplotter-hide: true` in a part's config JSON
/// (H6). Mirrors the web app's findComponentConfig check: the attribute is
/// truthy on the component's `attributes`. Garbage/missing config → empty set.
Set<String> hiddenComponentNames(String robotConfigJson) {
  if (robotConfigJson.isEmpty) return const {};
  try {
    final cfg = jsonDecode(robotConfigJson);
    final comps = (cfg as Map)['components'];
    if (comps is! List) return const {};
    final hidden = <String>{};
    for (final c in comps) {
      if (c is! Map) continue;
      final attrs = c['attributes'];
      if (attrs is! Map) continue;
      final hide = attrs['chartplotter-hide'];
      final truthy =
          hide == true || hide == 'true' || (hide is num && hide != 0);
      final name = c['name'];
      if (truthy && name is String && name.isNotEmpty) hidden.add(name);
    }
    return hidden;
  } catch (_) {
    return const {};
  }
}

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
  Viam? _viam; // present on the login path; enables cloud-data backfill

  /// The live robot connection, for features that fetch on demand (cameras).
  RobotClient? get robot => _robot;

  /// Cloud-data backfill for the detail graphs, or null in API-key mode.
  HistoryService? _history;
  HistoryService? get history => _history;
  String? _movementSensorName;
  String? _depthSensorName;
  String? _windSensorName;
  String? _seatempSensorName;
  String? _aisSensorName;
  String? _routeSensorName;
  String? _spotZeroFwName;
  String? _spotZeroSwName;
  String? _seakeeperName;
  // Heartbeat stand-in for machines with no movement sensor: without one the
  // poll loop had nothing to prove liveness with, so a dead connection was
  // never noticed at all.
  String? _fallbackSensorName;
  List<String> _tankNames = const [];
  // Last known reading per tank, keyed by leaf name — the source of truth for
  // carrying a tank's level and capacity across a failed read.
  //
  // Deliberately NOT `state.tanks`, which this used to read: that list is
  // rebuilt from scratch every cycle, so a tank whose read failed before it
  // had ever succeeded dropped out of it entirely. Once out, its carried
  // capacity was gone, and the sensor only reports `Capacity` intermittently —
  // so a tank that came back without one lost its fill/burn rate permanently
  // (the rate is computed in gallons and needs the capacity to exist).
  final Map<String, TankStatus> _tankLast = {};
  List<String> _acPowerNames = const [];
  Timer? _timer;
  int _tickN = 0;
  bool _polling = false;
  bool _ownsRobot = false; // close it on dispose only if we dialed it

  // ---- connection health ----------------------------------------------
  // gRPC calls on a dead WebRTC connection (e.g. after the phone was locked)
  // tend to HANG rather than fail, so every boat RPC below is deadline-bounded
  // and "the poll loop stopped succeeding" is the death signal. Two straight
  // failed heartbeats — or a poll that blows its whole budget — triggers a
  // re-dial via [reconnector].

  /// Heartbeat deadline: generous enough for a slow cellular/satellite link,
  /// short enough that two of them fit inside a few seconds.
  static const _heartbeatTimeout = Duration(seconds: 4);

  /// Deadline for the non-heartbeat sensor reads. These used to be unbounded,
  /// so one hung read wedged the whole poll loop until the watchdog fired.
  static const _sensorTimeout = Duration(seconds: 3);

  /// Whole-poll budget, so `_polling` can never latch even if several reads
  /// each hang for their own deadline.
  static const _tickBudget = Duration(seconds: 12);

  /// Probe deadline on app resume / manual retry — short, because we would
  /// rather re-dial than wait out a connection that is probably already dead.
  static const _probeTimeout = Duration(seconds: 3);

  /// Ceiling on one whole re-dial (token refresh + address lookup + WebRTC
  /// handshake). Without it a dial that never returns latches [_reconnecting]
  /// and the app sits on "reconnecting…" forever.
  static const _dialTimeout = Duration(seconds: 30);

  /// Wait before the *next* re-dial after N failures. The first attempt is
  /// immediate, so a momentary blip recovers in about a second; a long outage
  /// (out of range, boat powered down) backs off instead of hammering the radio.
  static const _backoff = <Duration>[
    Duration(seconds: 2),
    Duration(seconds: 4),
    Duration(seconds: 8),
    Duration(seconds: 15),
    Duration(seconds: 30),
  ];

  DateTime _lastGoodTick = DateTime.now();
  int _tickFailures = 0;
  bool _reconnecting = false;
  DateTime? _reconnectStartedAt;
  int _reconnectAttempts = 0;
  DateTime? _nextRetryAt;
  // Bumped on every re-dial. An attempt whose generation went stale closes its
  // client instead of installing it, so an abandoned dial can't resurrect a
  // connection that was stopped (or superseded by a forced retry) meanwhile.
  int _connectGen = 0;
  bool _paused = false; // app backgrounded: poll loop stopped
  bool _stopped = false; // stop()/dispose() ran: never reconnect again
  bool _started = false; // a first connect completed; the poll loop exists

  /// Re-dials the robot after the connection dies. Set by whoever connected
  /// us (main.dart via MachineConnector, or the API-key path internally);
  /// without it a dead connection just shows "Connection lost".
  Future<RobotClient> Function()? reconnector;

  Future<void> startWithRobot(RobotClient robot,
      {Viam? viam, String? robotId}) async {
    _robot = robot;
    _viam = viam;
    _robotId = robotId;
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
      try {
        _robot = await RobotClient.atAddress(
          Config.host,
          RobotClientOptions.withApiKey(Config.apiKeyId, Config.apiKey),
        );
      } catch (_) {
        // The mDNS-discovered local endpoint can be dead (the boat's
        // advertised ephemeral port isn't reachable) and the SDK's dial
        // mutates its options on the mDNS attempt, so its own fallback
        // redials the same dead endpoint. Retry with fresh options and mDNS
        // off to force the app.viam.com cloud path.
        state.setStatus('Local connect failed, retrying via cloud…');
        final options = RobotClientOptions.withApiKey(
          Config.apiKeyId,
          Config.apiKey,
        )..dialOptions.attemptMdns = false;
        _robot = await RobotClient.atAddress(Config.host, options);
      }
      _ownsRobot = true;
      // Dead-connection recovery redials the proven cloud path directly.
      reconnector = () {
        final options = RobotClientOptions.withApiKey(
          Config.apiKeyId,
          Config.apiKey,
        )..dialOptions.attemptMdns = false;
        return RobotClient.atAddress(Config.host, options);
      };
    } catch (e) {
      state.setStatus('Connect failed: $e');
      return;
    }
    await _afterConnect();
  }

  // Components the operator marked `chartplotter-hide` in the machine config
  // (H6). Fetched once per connect on the login path; the API-key path has no
  // cloud client and degrades to no filtering.
  String? _robotId;
  Set<String> _hiddenComponents = const {};

  Future<void> _loadHiddenComponents() async {
    final viam = _viam;
    final robotId = _robotId;
    if (viam == null || robotId == null) return;
    try {
      final parts = await viam.appClient.listRobotParts(robotId);
      final hidden = <String>{};
      for (final part in parts) {
        hidden.addAll(hiddenComponentNames(part.robotConfigJson));
      }
      _hiddenComponents = hidden;
    } catch (_) {
      // Config unreachable — discover unfiltered rather than fail connect.
    }
  }

  /// True when [resourceName] (possibly remote-prefixed, `remote:name`) is
  /// marked hidden in the machine config.
  bool _isHidden(String resourceName) =>
      _hiddenComponents.contains(resourceName.split(':').last);

  Future<void> _afterConnect() async {
    final robot = _robot;
    if (robot == null) return;
    await _loadHiddenComponents();
    await _discover(robot);
    _setConnectedStatus();
    await _buildHistory(robot);
    _started = true;
    _startTimer();
  }

  /// Resolve the component names we poll. RPC-free (the SDK caches
  /// `resourceNames` on the client) and therefore cheap enough to re-run after
  /// every re-dial — otherwise a viam-server restart that renamed or dropped a
  /// component would leave us polling names that no longer exist.
  Future<void> _discover(RobotClient robot) async {
    _movementSensorName = await _discoverMovementSensor(robot);
    _depthSensorName = _discoverSensorByName(robot, 'depth', Config.depthSensor);
    _windSensorName = _discoverSensorByName(robot, 'wind', '');
    _seatempSensorName = _discoverSensorByName(robot, 'seatemp', '');
    _aisSensorName = _discoverSensorEndingWith(robot, 'ais');
    _routeSensorName = _discoverSensorEndingWith(robot, 'route');
    _spotZeroFwName = _discoverSensorByName(robot, 'spotzero-fw', '');
    _spotZeroSwName = _discoverSensorByName(robot, 'spotzero-sw', '');
    _seakeeperName = _discoverSensorByName(robot, 'seakeeper', '');
    _tankNames = _tankSort(_discoverSensorsWhere(
        robot, (n) => n.contains('fuel') || n.contains('freshwater')));
    _acPowerNames =
        _discoverSensorsWhere(robot, (n) => RegExp(r'ac-\d-\d$').hasMatch(n));
    _fallbackSensorName = _movementSensorName != null
        ? null
        : (_depthSensorName ??
            _windSensorName ??
            _seatempSensorName ??
            (_tankNames.isEmpty ? null : _tankNames.first));
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
  }

  void _startTimer() {
    _timer?.cancel();
    _timer = Timer.periodic(const Duration(seconds: 1), (_) => _tick());
  }

  void _setConnectedStatus() {
    state.setConnectionDiag(error: null, attempts: 0);
    state.setStatus(
        _movementSensorName == null ? 'Connected — no GPS' : 'Connected');
  }

  /// Assemble the cloud-data backfill for the detail graphs: the machine's
  /// cloud identity (from the robot) plus a capture spec per graphable metric.
  /// Only possible on the login path, where we hold a [Viam] handle.
  Future<void> _buildHistory(RobotClient robot) async {
    final viam = _viam;
    if (viam == null) return;
    try {
      final meta = await robot.getCloudMetadata();
      String leaf(String? n) => (n ?? '').split(':').last;
      final specs = <String, HistorySpec>{};
      if (_depthSensorName != null) {
        specs['depth'] = HistorySpec(
            leaf(_depthSensorName), 'Depth', (v) => v * 3.28084);
      }
      if (_seatempSensorName != null) {
        specs['seatemp'] = HistorySpec(
            leaf(_seatempSensorName), 'Temperature', (v) => 32 + v * 1.8);
      }
      if (_windSensorName != null) {
        specs['wind'] = HistorySpec(
          leaf(_windSensorName),
          'Wind Speed',
          (v) => v * 1.94384,
          extraMatch: const {
            'data.readings.Reference': 'True (ground referenced to North)',
          },
        );
      }
      for (final tn in _tankNames) {
        specs['tank:${leaf(tn)}'] =
            HistorySpec(leaf(tn), 'Level', (v) => v);
      }
      _history = HistoryService(
        dataClient: viam.dataClient,
        orgId: meta.primaryOrgId,
        locationId: meta.locationId,
        robotId: meta.machineId,
        specs: specs,
      );
    } catch (_) {
      // No cloud metadata / data access → graphs stay live-only.
    }
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
        if (_isHidden(rn.name)) continue;
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
        if (_isHidden(rn.name)) continue;
        if (rn.subtype == 'sensor' && pred(rn.name.toLowerCase())) {
          out.add(rn.name);
        }
      }
    } catch (_) {}
    out.sort();
    return out;
  }

  bool _boolish(dynamic v) => v == true || (v is num && v != 0);

  /// Port of the web app's tankSort: freshwater first, then fwd/mid/aft, main
  /// later; ties broken by name.
  List<String> _tankSort(List<String> names) {
    int score(String raw) {
      final n = raw.toLowerCase();
      var s = 0;
      if (n.contains('freshwater')) s -= 10000;
      if (n.contains('fwd')) s -= 1000;
      if (n.contains('mid')) s -= 500;
      if (n.contains('aft')) s -= 250;
      if (n.contains('main')) s += 50;
      return s;
    }

    return List.of(names)
      ..sort((a, b) {
        final d = score(a) - score(b);
        return d != 0 ? d : a.toLowerCase().compareTo(b.toLowerCase());
      });
  }

  /// Camera components on the robot, sorted, with the web app's filtered-camera
  /// rule: drop "<name>" when a "<name>-filtered" sibling exists (prefer the
  /// filtered feed). (chartplotter-hide needs the machine config; not yet done.)
  List<String> _discoverCameras(RobotClient robot) {
    final all = <String>[];
    try {
      for (final rn in robot.resourceNames) {
        if (_isHidden(rn.name)) continue;
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
        if (_isHidden(rn.name)) continue;
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
        if (_isHidden(rn.name)) continue;
        if (rn.subtype == 'movement_sensor') return rn.name;
      }
    } catch (_) {
      // resourceNames shape can vary across SDK versions; fall through.
    }
    return null;
  }

  Future<void> _tick() async {
    if (_stopped || _paused) return;
    final robot = _robot;
    if (robot == null) {
      // A failed reconnect leaves no robot — without this, the loop would
      // go quiet forever. Keep re-dialing (spaced by the backoff inside
      // _reconnect) until the boat is back or stop() cancels the timer.
      unawaited(_reconnect());
      _showRetryCountdown();
      return;
    }
    final msName = _movementSensorName;
    if (msName == null && _fallbackSensorName == null) return;
    if (_polling) {
      // A previous poll is stuck in a hung RPC — the signature of a dead
      // WebRTC connection. The poll body is budget-bounded so this is
      // normally transient; declare death once nothing has succeeded for a
      // while (the reconnect closes the robot, which unblocks the hung calls).
      if (DateTime.now().difference(_lastGoodTick) > _tickBudget) {
        unawaited(_reconnect());
      }
      return;
    }
    _polling = true;
    _tickN++;
    try {
      await _pollOnce(robot, msName).timeout(_tickBudget);
    } catch (_) {
      // Budget blown: abandon the poll rather than let it hold the loop
      // hostage. The heartbeat was already recorded inside, so the watchdog's
      // verdict doesn't depend on the sweep finishing.
    } finally {
      _polling = false;
    }
  }

  /// Record the heartbeat outcome — called from inside the poll the moment it
  /// is known, so a slow boat-systems sweep can't be mistaken for a dead
  /// connection and a dead one is acted on without waiting for the sweep.
  void _noteHeartbeat(bool ok) {
    if (_stopped) return;
    if (ok) {
      _lastGoodTick = DateTime.now();
      _tickFailures = 0;
      _reconnectAttempts = 0;
      _nextRetryAt = null;
      if (!state.connected && !_reconnecting) _setConnectedStatus();
    } else {
      _tickFailures++;
      // Two straight failed heartbeats = the connection is gone (a healthy
      // boat answers within the deadline even when other sensors error).
      if (_tickFailures >= 2) unawaited(_reconnect());
    }
  }

  /// One poll pass over the boat's sensors.
  ///
  /// Every call here is deadline-bounded: an unbounded gRPC call on a dead
  /// WebRTC peer hangs rather than failing, and a single hung read used to
  /// wedge the loop until a 15 s watchdog noticed.
  Future<void> _pollOnce(RobotClient robot, String? msName) async {
    var ok = false;
    // The heartbeat blew its deadline rather than being refused. That means
    // the link is hung, not that one sensor is unhappy — nothing else on this
    // connection will answer either, so abandon the sweep instead of spending
    // the tick budget on it. (Getting this wrong is what made recovery slow:
    // the next heartbeat couldn't start until every dead read had timed out.)
    var linkHung = false;
    if (msName != null) {
      final ms = MovementSensor.fromRobot(robot, msName);

      try {
        // Heartbeat call: bounded so a dead connection fails fast instead of
        // wedging the poll loop forever.
        final p = await ms.position().timeout(_heartbeatTimeout);
        // Position exposes `coordinates` (a GeoPoint), not `coordinate`.
        state.update(
          position: LatLng(p.coordinates.latitude, p.coordinates.longitude),
        );
        ok = true;
      } on TimeoutException {
        linkHung = true;
      } catch (_) {}
      _noteHeartbeat(ok);
      if (linkHung) return;

      try {
        final v = await ms.linearVelocity().timeout(_sensorTimeout);
        state.update(speedKn: v.y * 1.94384);
      } catch (_) {}

      try {
        final h = await ms.compassHeading().timeout(_sensorTimeout);
        state.update(headingDeg: h);
      } catch (_) {}

      // Course over ground lives in the movement sensor's generic readings
      // under one of several key spellings (matches the web app).
      try {
        final rd = await ms.readings().timeout(_sensorTimeout);
        final cog = rd['Course Over Ground'] ??
            rd['course_over_ground'] ??
            rd['CourseOverGround'] ??
            rd['cog'] ??
            rd['COG'];
        if (cog is num) state.update(cogDeg: cog.toDouble());
      } catch (_) {}
    } else {
      // No movement sensor on this machine — keep a pulse on whatever sensor
      // we did find, so the watchdog still has something to declare dead.
      final hb = _fallbackSensorName;
      if (hb != null) {
        try {
          await Sensor.fromRobot(robot, hb)
              .readings()
              .timeout(_heartbeatTimeout);
          ok = true;
        } on TimeoutException {
          linkHung = true;
        } catch (_) {}
        _noteHeartbeat(ok);
        if (linkHung) return;
      }
    }

    // Boat systems (watermaker / seakeeper / tanks / AC power) change slowly,
    // so they poll on the slow cadence — but ahead of the per-tick sensors,
    // not behind them. Sitting at the tail meant that on a link slow enough
    // for each read to approach its deadline, the tick budget ran out first
    // and the tank readings simply stopped arriving — taking the fuel rate
    // with them, right after a reconnect when the link is at its worst.
    if (_tickN % 5 == 2) await _pollSystems(robot);

    final windName = _windSensorName;
    if (windName != null) {
      try {
        final r = await Sensor.fromRobot(robot, windName)
            .readings()
            .timeout(_sensorTimeout);
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
        final r = await Sensor.fromRobot(robot, tempName)
            .readings()
            .timeout(_sensorTimeout);
        final t = r['Temperature']; // °C → °F
        if (t is num) state.update(seaTempF: 32 + t.toDouble() * 1.8);
      } catch (_) {}
    }

    final depthName = _depthSensorName;
    if (depthName != null) {
      try {
        final r = await Sensor.fromRobot(robot, depthName)
            .readings()
            .timeout(_sensorTimeout);
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
        final r = await Sensor.fromRobot(robot, aisName)
            .readings()
            .timeout(_sensorTimeout);
        state.setAis(parseAisReadings(r));
      } catch (_) {}
    }

    // Active route destination from the `route` sensor (rarely changes, so
    // poll on the slow cadence). Cleared when there's no active route.
    final routeName = _routeSensorName;
    if (routeName != null && _tickN % 5 == 0) {
      try {
        final r = await Sensor.fromRobot(robot, routeName)
            .readings()
            .timeout(_sensorTimeout);
        // Keys match the web app's route sensor (spaced/capitalised).
        final lat = r['Destination Latitude'];
        final lon = r['Destination Longitude'];
        final dist = r['Distance to Waypoint']; // metres
        final closing = r['Waypoint Closing Velocity']; // m/s
        state.setRoute(
          destination: (lat is num && lon is num)
              ? LatLng(lat.toDouble(), lon.toDouble())
              : null,
          wpDistanceM: dist is num ? dist.toDouble() : null,
          wpClosingMs: closing is num ? closing.toDouble() : null,
        );
      } catch (_) {}
    }
  }

  /// How long to wait before the next re-dial, given [attempts] failures so
  /// far. Saturates at the last [_backoff] entry. Public so the schedule is
  /// unit-testable — nothing else should need it.
  static Duration reconnectBackoff(int attempts) {
    final i = attempts - 1;
    if (i < 0) return _backoff.first;
    if (i >= _backoff.length) return _backoff.last;
    return _backoff[i];
  }

  /// Keep the status pill honest while we sit out the reconnect backoff.
  void _showRetryCountdown() {
    if (_reconnecting || _stopped) return;
    final at = _nextRetryAt;
    if (at == null) return;
    final secs = at.difference(DateTime.now()).inSeconds;
    if (secs > 0) state.setStatus('Offline — retrying in ${secs}s');
  }

  /// Probe the connection now — called on app resume, when a connection
  /// killed while the phone was locked would otherwise sit around looking
  /// "Connected" until the watchdog notices. Re-dials if the probe fails.
  ///
  /// [assumeDead] skips the probe and re-dials straight away: after a real
  /// background stint the peer is almost always gone, and waiting out a probe
  /// we expect to fail just delays recovery by its whole deadline.
  Future<void> checkNow({bool assumeDead = false}) async {
    if (_stopped) return;
    final robot = _robot;
    if (robot == null) {
      await _reconnect(force: true);
      return;
    }
    if (_reconnecting) {
      // Let _reconnect judge it: an in-flight dial that's still within its
      // deadline is left alone, a stuck one is taken over.
      await _reconnect(force: true);
      return;
    }
    if (!assumeDead) {
      final msName = _movementSensorName;
      final hb = _fallbackSensorName;
      try {
        if (msName != null) {
          await MovementSensor.fromRobot(robot, msName)
              .position()
              .timeout(_probeTimeout);
        } else if (hb != null) {
          await Sensor.fromRobot(robot, hb).readings().timeout(_probeTimeout);
        } else {
          // Nothing to probe with — let a re-dial settle it.
          await _reconnect(force: true);
          return;
        }
        _lastGoodTick = DateTime.now();
        _tickFailures = 0;
        _reconnectAttempts = 0;
        _nextRetryAt = null;
        _setConnectedStatus();
        return; // alive
      } catch (_) {}
    }
    await _reconnect(force: true);
  }

  /// User-initiated retry from the status pill: skip both the probe and the
  /// backoff wait.
  Future<void> reconnectNow() => checkNow(assumeDead: true);

  /// App backgrounded: stop the poll loop. The OS suspends (iOS) or throttles
  /// (Android) us anyway, so ticking on only burns battery and cellular data
  /// and fills the failure counters with noise.
  void pause() {
    if (_stopped) return;
    _paused = true;
    _timer?.cancel();
    _timer = null;
  }

  /// App foregrounded: restart the poll loop and settle the connection's fate
  /// immediately instead of waiting for the watchdog.
  Future<void> resume({bool assumeDead = false}) async {
    if (_stopped) return;
    _paused = false;
    if (_started && _timer == null) _startTimer();
    await checkNow(assumeDead: assumeDead);
  }

  /// Close the (dead) robot and dial a fresh one via [reconnector].
  ///
  /// The 1 s poll timer keeps calling in while there's no robot; [_nextRetryAt]
  /// spaces real dial attempts on the [_backoff] schedule — the first retry is
  /// immediate, so a blip recovers in about a second, and a long outage backs
  /// off to 30 s instead of hammering the radio. [force] (app resume, manual
  /// retry) skips the wait and resets the backoff.
  Future<void> _reconnect({bool force = false}) async {
    if (_stopped) return;
    final rec = reconnector;
    if (rec == null) {
      state.setStatus('Connection lost');
      return;
    }
    if (_reconnecting) {
      // A dial that never returns would latch this flag and wedge every future
      // attempt — the app then sits on "reconnecting…" forever. Past the
      // deadline, orphan it; the generation bump makes its result a no-op.
      final since = _reconnectStartedAt;
      if (since != null &&
          DateTime.now().difference(since) < _dialTimeout * 2) {
        return;
      }
      _connectGen++;
      _reconnecting = false;
      _reconnectStartedAt = null;
    }
    if (force) {
      _reconnectAttempts = 0;
      _nextRetryAt = null;
    } else {
      final next = _nextRetryAt;
      if (next != null && DateTime.now().isBefore(next)) return;
    }

    final gen = ++_connectGen;
    _reconnecting = true;
    _reconnectStartedAt = DateTime.now();
    _reconnectAttempts++;
    state.setConnectionDiag(
        error: state.lastConnectError, attempts: _reconnectAttempts);
    state.setStatus(_reconnectAttempts > 1
        ? 'Reconnecting… (try $_reconnectAttempts)'
        : 'Reconnecting…');
    try {
      final old = _robot;
      _robot = null;
      // Closing unblocks any RPCs hung on the dead connection — but close()
      // can itself hang on a dead peer, so bound it too.
      try {
        await old?.close().timeout(const Duration(seconds: 3));
      } catch (_) {}
      final fresh = await rec().timeout(_dialTimeout);
      if (gen != _connectGen || _stopped) {
        // stop() ran, or a forced retry superseded us, while we were dialing —
        // don't resurrect a connection nobody owns.
        try {
          await fresh.close();
        } catch (_) {}
        return;
      }
      _robot = fresh;
      _tickFailures = 0;
      _lastGoodTick = DateTime.now();
      _reconnectAttempts = 0;
      _nextRetryAt = null;
      // A re-dial can land on a viam-server that restarted with a different
      // component set; re-resolve the names before polling them.
      await _discover(fresh);
      _setConnectedStatus();
    } catch (e) {
      if (gen == _connectGen) {
        final wait = reconnectBackoff(_reconnectAttempts);
        _nextRetryAt = DateTime.now().add(wait);
        // The pill is too narrow for a gRPC error, so it gets the countdown
        // and Debug gets the reason.
        state.setConnectionDiag(error: '$e', attempts: _reconnectAttempts);
        state.setStatus('Offline — retrying in ${wait.inSeconds}s');
      }
    } finally {
      if (gen == _connectGen) {
        _reconnecting = false;
        _reconnectStartedAt = null;
      }
    }
  }

  /// Record a tank reading as the carry-forward baseline and hand it back, so
  /// the value survives a cycle in which its read fails.
  TankStatus _rememberTank(TankStatus t) {
    _tankLast[t.name] = t;
    return t;
  }

  /// Poll the boat-systems sensors (all generic Sensor readings), mirroring the
  /// web app's data panel: watermaker flow, seakeeper, tank levels, AC power.
  Future<void> _pollSystems(RobotClient robot) async {
    final fwName = _spotZeroFwName;
    if (fwName != null) {
      try {
        final r = await Sensor.fromRobot(robot, fwName)
            .readings()
            .timeout(_sensorTimeout);
        final f = r['Product Water Flow'];
        if (f is num) {
          state.setSystems(spotZeroFwGph: f.toDouble() * 0.00440287);
        }
      } catch (_) {}
    }
    final swName = _spotZeroSwName;
    if (swName != null) {
      try {
        final r = await Sensor.fromRobot(robot, swName)
            .readings()
            .timeout(_sensorTimeout);
        final f = r['Product Water Flow'];
        if (f is num) {
          state.setSystems(spotZeroSwGph: f.toDouble() * 0.00440287);
        }
      } catch (_) {}
    }
    final skName = _seakeeperName;
    if (skName != null) {
      try {
        final r = await Sensor.fromRobot(robot, skName)
            .readings()
            .timeout(_sensorTimeout);
        final prog = r['progress_bar_percentage'];
        state.setSystems(
          seakeeperStabilizing: _boolish(r['stabilize_enabled']),
          seakeeperPower: _boolish(r['power_enabled']),
          seakeeperProgress: prog is num ? prog.toDouble() : null,
        );
      } catch (_) {}
    }
    if (_tankNames.isNotEmpty) {
      final tanks = <TankStatus>[];
      for (final tn in _tankNames) {
        final name = tn.split(':').last;
        final old = _tankLast[name];
        try {
          final r = await Sensor.fromRobot(robot, tn)
              .readings()
              .timeout(_sensorTimeout);
          final lvl = r['Level'];
          final cap = r['Capacity'];
          // Boat-side data arrival time (viamboat's boat-sensor
          // `_last_update`; absent on older module builds). A timestamp
          // that's present but unparseable, absurdly old (Go zero time), or
          // in the future (beyond a little clock skew) is flagged invalid
          // rather than trusted.
          DateTime? boatAt;
          var boatInvalid = false;
          final lu = r['_last_update'];
          if (lu != null) {
            final parsed = lu is String ? DateTime.tryParse(lu) : null;
            if (parsed == null ||
                parsed.year < 2000 ||
                parsed.isAfter(
                    DateTime.now().add(const Duration(seconds: 5)))) {
              boatInvalid = true;
            } else {
              boatAt = parsed.toLocal();
            }
          }
          final level = lvl is num ? lvl.toDouble() : old?.level;
          if (level == null) continue; // not a tank (e.g. freshwater-relay)
          // No `_last_update` (module build predates it): a successful read
          // still proves the boat-side data is <60 s old — viamboat errors
          // beyond that — so "now" keeps the staleness color correct.
          tanks.add(_rememberTank(TankStatus(
            name: name,
            level: level,
            capacity: cap is num ? cap.toDouble() : old?.capacity,
            fetchedAt: DateTime.now(),
            boatUpdatedAt: boatInvalid ? null : (boatAt ?? DateTime.now()),
            boatTimestampInvalid: boatInvalid,
          )));
        } catch (e) {
          // viamboat fails Readings once boat-side data is >60 s old ("too
          // old"). That RPC still reached the boat, so OUR fetch is fresh —
          // only the boat-side age keeps growing. When the timestamp was
          // never seen (older module build, or stale since before we
          // connected), synthesize now-60 s so the age keeps counting from
          // the threshold. Any other error means we couldn't reach the
          // boat: leave both timestamps aging.
          final reachedBoat = e.toString().contains('too old');
          if (old == null && !reachedBoat) continue;
          final wasInvalid = old?.boatTimestampInvalid ?? false;
          tanks.add(_rememberTank(TankStatus(
            name: name,
            level: old?.level,
            capacity: old?.capacity,
            fetchedAt: reachedBoat ? DateTime.now() : old?.fetchedAt,
            // A previously-invalid timestamp stays invalid — synthesizing an
            // age would launder the bad clock into a plausible-looking one.
            boatUpdatedAt: wasInvalid
                ? null
                : reachedBoat
                    ? (old?.boatUpdatedAt ??
                        DateTime.now().subtract(const Duration(seconds: 60)))
                    : old?.boatUpdatedAt,
            boatTimestampInvalid: wasInvalid,
            // Nothing was measured this cycle — this level is the last known
            // one carried forward, so it must not enter the history series.
            levelIsFresh: false,
          )));
        }
      }
      if (tanks.isNotEmpty) state.setSystems(tanks: tanks);
    }
    if (_acPowerNames.isNotEmpty) {
      double vSum = 0;
      int vN = 0;
      double wSum = 0;
      for (final an in _acPowerNames) {
        try {
          final r = await Sensor.fromRobot(robot, an)
              .readings()
              .timeout(_sensorTimeout);
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

  /// Stop polling and close the robot connection unconditionally — used when
  /// switching boats, where nothing else will close a picker-dialed robot.
  Future<void> stop() async {
    // Set before cancelling: an in-flight re-dial checks this (and its
    // generation) before installing its client, so it can't resurrect a
    // connection nobody owns.
    _stopped = true;
    _connectGen++;
    _timer?.cancel();
    _timer = null;
    final r = _robot;
    _robot = null;
    await r?.close();
  }

  Future<void> dispose() async {
    _stopped = true;
    _connectGen++;
    _timer?.cancel();
    _timer = null;
    if (_ownsRobot) await _robot?.close();
  }
}
