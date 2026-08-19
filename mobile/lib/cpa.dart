import 'dart:math' as math;

/// Closest point of approach (D3) — ported verbatim from the web app's
/// computeCpa (src/marineMap.svelte:5147). The single most safety-relevant
/// AIS readout: CPA and time-to-CPA are how you decide whether a crossing
/// target is a problem.
///
/// Flat-earth projection around own position, relative velocity from both
/// COGs and SOGs, `tcpa = -(d·dv)/|dv|²`. Null when either COG is missing or
/// there's effectively no relative motion. Callers show it only when
/// `tcpaMin >= 0` — a target already past its CPA isn't a threat.
({double cpaNm, double tcpaMin})? computeCpa({
  required double ownLat,
  required double ownLng,
  required double? ownCogDeg,
  required double ownSpdKn,
  required double tgtLat,
  required double tgtLng,
  required double? tgtCogDeg,
  required double tgtSpdKn,
}) {
  if (ownCogDeg == null || tgtCogDeg == null) return null;
  if (!ownSpdKn.isFinite || !tgtSpdKn.isFinite) return null;
  final lat0 = ownLat * math.pi / 180;
  final mPerDegLat = 111132.92 - 559.82 * math.cos(2 * lat0);
  final mPerDegLng = 111412.84 * math.cos(lat0);
  final dN = (tgtLat - ownLat) * mPerDegLat;
  final dE = (tgtLng - ownLng) * mPerDegLng;
  const knToMs = 0.514444;
  final ownVN = ownSpdKn * knToMs * math.cos(ownCogDeg * math.pi / 180);
  final ownVE = ownSpdKn * knToMs * math.sin(ownCogDeg * math.pi / 180);
  final tgtVN = tgtSpdKn * knToMs * math.cos(tgtCogDeg * math.pi / 180);
  final tgtVE = tgtSpdKn * knToMs * math.sin(tgtCogDeg * math.pi / 180);
  final dvN = tgtVN - ownVN;
  final dvE = tgtVE - ownVE;
  final dvSq = dvN * dvN + dvE * dvE;
  if (dvSq < 1e-6) return null; // no relative motion
  final tcpaSec = -(dN * dvN + dE * dvE) / dvSq;
  final futN = dN + dvN * tcpaSec;
  final futE = dE + dvE * tcpaSec;
  final cpaM = math.sqrt(futN * futN + futE * futE);
  return (cpaNm: cpaM / 1852, tcpaMin: tcpaSec / 60);
}
