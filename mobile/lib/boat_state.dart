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
  List<AisBoat> aisBoats = const [];
  // Which component each reading came from (for the drawer's Sources section).
  Map<String, String?> sources = const {};
  String status = 'Starting…';
  DateTime? lastUpdate;

  bool get connected => status.startsWith('Connected');

  void setAis(List<AisBoat> boats) {
    aisBoats = boats;
    notifyListeners();
  }

  void setSources(Map<String, String?> s) {
    sources = s;
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
    if (speedKn != null) this.speedKn = speedKn;
    if (cogDeg != null) this.cogDeg = cogDeg;
    if (headingDeg != null) this.headingDeg = headingDeg;
    if (depthFt != null) this.depthFt = depthFt;
    if (seaTempF != null) this.seaTempF = seaTempF;
    if (windSpeedKn != null) this.windSpeedKn = windSpeedKn;
    if (windAngleDeg != null) this.windAngleDeg = windAngleDeg;
    lastUpdate = DateTime.now();
    notifyListeners();
  }
}
