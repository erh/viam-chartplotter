import 'package:flutter/foundation.dart';
import 'package:latlong2/latlong.dart';

/// Snapshot of the live boat readings the spike polls. A ChangeNotifier so
/// the map screen rebuilds on each 1 Hz tick. v1 will likely move this into
/// Riverpod and add AIS, wind, sea temp, gauges, routes, etc.
class BoatState extends ChangeNotifier {
  LatLng? position;
  double? speedKn; // from linear velocity
  double? headingDeg; // compass heading
  double? depthFt; // optional, if a depth sensor is configured
  String status = 'Starting…';
  DateTime? lastUpdate;

  void setStatus(String s) {
    status = s;
    notifyListeners();
  }

  void update({
    LatLng? position,
    double? speedKn,
    double? headingDeg,
    double? depthFt,
  }) {
    if (position != null) this.position = position;
    if (speedKn != null) this.speedKn = speedKn;
    if (headingDeg != null) this.headingDeg = headingDeg;
    if (depthFt != null) this.depthFt = depthFt;
    lastUpdate = DateTime.now();
    notifyListeners();
  }
}
