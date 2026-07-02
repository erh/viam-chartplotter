import 'package:flutter/foundation.dart';
import 'package:latlong2/latlong.dart';

import 'ais.dart';

/// Snapshot of the live boat readings the app polls. A ChangeNotifier so the
/// UI rebuilds on each 1 Hz tick. Grows as more of the web app's readouts are
/// ported (AIS, gauges, routes, …).
class BoatState extends ChangeNotifier {
  LatLng? position;
  double? speedKn; // SOG, from linear velocity
  double? cogDeg; // course over ground
  double? headingDeg; // compass heading
  double? depthFt;
  double? seaTempF;
  double? windSpeedKn; // true wind
  double? windAngleDeg; // true wind angle
  // Boat systems (from the web app's data panel).
  double? spotZeroFwGph; // watermaker fresh-water product flow
  double? spotZeroSwGph; // watermaker sea-water flow
  bool? seakeeperStabilizing;
  bool? seakeeperPower;
  double? seakeeperProgress; // spool-up %
  List<({String name, double level})> tanks = const []; // fuel / fresh water
  double? acVolts; // average line-neutral voltage
  double? acWatts; // total AC load
  List<AisBoat> aisBoats = const [];
  List<String> cameraNames = const [];
  LatLng? destination; // active route destination (from the `route` sensor)
  double? wpDistanceM; // distance to next waypoint, metres
  double? wpClosingMs; // closing velocity toward the waypoint, m/s
  // Which component each reading came from (for the drawer's Sources section).
  Map<String, String?> sources = const {};
  String windInfo = 'off'; // wind-overlay fetch state, shown in Debug
  String status = 'Starting…';
  DateTime? lastUpdate;

  // Timestamped live history per metric (depth/seatemp/sog/wind/tank:<name>),
  // for the inline sparklines and the detail graph. ~4h at 1 Hz.
  final Map<String, List<({DateTime t, double v})>> history = {};
  static const int _histCap = 15000;

  bool get connected => status.startsWith('Connected');

  /// True when there's an active waypoint to navigate to.
  bool get navigating => (wpDistanceM ?? 0) > 0;

  /// Distance to the next waypoint in nautical miles.
  double? get wpDistanceNm =>
      wpDistanceM == null ? null : wpDistanceM! * 0.000539957;

  /// Minutes to the next waypoint at the current closing velocity, or null when
  /// not closing (stationary / opening). Mirrors the web app's calc.
  double? get wpEtaMinutes =>
      (wpDistanceM != null && wpClosingMs != null && wpClosingMs! > 0)
          ? wpDistanceM! / wpClosingMs! / 60
          : null;

  /// Wall-clock arrival time, or null when ETA is unknown.
  DateTime? get wpEta {
    final m = wpEtaMinutes;
    return m == null
        ? null
        : DateTime.now().add(Duration(seconds: (m * 60).round()));
  }

  /// Last ~60 values for the inline sparkline glance.
  List<double> spark(String key) {
    final l = history[key];
    if (l == null) return const [];
    final start = l.length > 60 ? l.length - 60 : 0;
    return [for (var i = start; i < l.length; i++) l[i].v];
  }

  /// Full timestamped series for the detail graph.
  List<({DateTime t, double v})> series(String key) =>
      history[key] ?? const [];

  void _push(String key, double v) {
    final list = history.putIfAbsent(key, () => []);
    list.add((t: DateTime.now(), v: v));
    if (list.length > _histCap) list.removeAt(0);
  }

  void setAis(List<AisBoat> boats) {
    aisBoats = boats;
    notifyListeners();
  }

  void setRoute({
    LatLng? destination,
    double? wpDistanceM,
    double? wpClosingMs,
  }) {
    this.destination = destination;
    this.wpDistanceM = wpDistanceM;
    this.wpClosingMs = wpClosingMs;
    notifyListeners();
  }

  void setSources(Map<String, String?> s) {
    sources = s;
    notifyListeners();
  }

  void setCameras(List<String> names) {
    cameraNames = names;
    notifyListeners();
  }

  void setWindInfo(String s) {
    windInfo = s;
    notifyListeners();
  }

  void setSystems({
    double? spotZeroFwGph,
    double? spotZeroSwGph,
    bool? seakeeperStabilizing,
    bool? seakeeperPower,
    double? seakeeperProgress,
    List<({String name, double level})>? tanks,
    double? acVolts,
    double? acWatts,
  }) {
    if (spotZeroFwGph != null) this.spotZeroFwGph = spotZeroFwGph;
    if (spotZeroSwGph != null) this.spotZeroSwGph = spotZeroSwGph;
    if (seakeeperStabilizing != null) {
      this.seakeeperStabilizing = seakeeperStabilizing;
    }
    if (seakeeperPower != null) this.seakeeperPower = seakeeperPower;
    if (seakeeperProgress != null) this.seakeeperProgress = seakeeperProgress;
    if (tanks != null) {
      this.tanks = tanks;
      for (final t in tanks) {
        _push('tank:${t.name}', t.level);
      }
    }
    if (acVolts != null) this.acVolts = acVolts;
    if (acWatts != null) this.acWatts = acWatts;
    notifyListeners();
  }

  void setStatus(String s) {
    status = s;
    notifyListeners();
  }

  void update({
    LatLng? position,
    double? speedKn,
    double? cogDeg,
    double? headingDeg,
    double? depthFt,
    double? seaTempF,
    double? windSpeedKn,
    double? windAngleDeg,
  }) {
    if (position != null) this.position = position;
    if (speedKn != null) {
      this.speedKn = speedKn;
      _push('sog', speedKn);
    }
    if (cogDeg != null) this.cogDeg = cogDeg;
    if (headingDeg != null) this.headingDeg = headingDeg;
    if (depthFt != null) {
      this.depthFt = depthFt;
      _push('depth', depthFt);
    }
    if (seaTempF != null) {
      this.seaTempF = seaTempF;
      _push('seatemp', seaTempF);
    }
    if (windSpeedKn != null) {
      this.windSpeedKn = windSpeedKn;
      _push('wind', windSpeedKn);
    }
    if (windAngleDeg != null) this.windAngleDeg = windAngleDeg;
    lastUpdate = DateTime.now();
    notifyListeners();
  }
}
