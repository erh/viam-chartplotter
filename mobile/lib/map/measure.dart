import 'dart:math' as math;

import 'package:latlong2/latlong.dart';

/// Measure-tool math (J1), shared with the heading line and CPA work.

/// Initial bearing from [from] to [to] in degrees [0, 360) — the standard
/// forward-azimuth formula, mirroring the web app's bearingDeg
/// (src/marineMap.svelte:511). No more accuracy than a helmsman can steer.
double bearingDeg(LatLng from, LatLng to) {
  double toRad(double d) => d * math.pi / 180;
  final phi1 = toRad(from.latitude);
  final phi2 = toRad(to.latitude);
  final dLambda = toRad(to.longitude - from.longitude);
  final y = math.sin(dLambda) * math.cos(phi2);
  final x = math.cos(phi1) * math.sin(phi2) -
      math.sin(phi1) * math.cos(phi2) * math.cos(dLambda);
  return (math.atan2(y, x) * 180 / math.pi + 360) % 360;
}

/// Distance in nautical miles.
double distanceNm(LatLng from, LatLng to) =>
    const Distance().distance(from, to) / 1852.0;

/// Readout for the measure pill: "10.00 nm @ 057°T".
String measureLabel(LatLng from, LatLng to) {
  final nm = distanceNm(from, to);
  final brg = bearingDeg(from, to).round() % 360;
  return '${nm.toStringAsFixed(2)} nm @ ${brg.toString().padLeft(3, '0')}°T';
}
