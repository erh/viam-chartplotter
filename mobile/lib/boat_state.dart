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
  // Which component each reading came from (for the drawer's Sources section).
  Map<String, String?> sources = const {};
  String windInfo = 'off'; // wind-overlay fetch state, shown in Debug
  String status = 'Starting…';
  DateTime? lastUpdate;

  // Rolling live-value history for sparklines, keyed by metric
  // (depth/seatemp/sog/wind/tank:<name>). Live-accumulated as values arrive.
  final Map<String, List<double>> history = {};
  static const int _histCap = 150;

  bool get connected => status.startsWith('Connected');

  List<double>? spark(String key) => history[key];

  void _push(String key, double v) {
    final list = history.putIfAbsent(key, () => <double>[]);
    list.add(v);
    if (list.length > _histCap) list.removeAt(0);
  }

  void setAis(List<AisBoat> boats) {
    aisBoats = boats;
    notifyListeners();
  }

  void setDestination(LatLng? d) {
    destination = d;
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
