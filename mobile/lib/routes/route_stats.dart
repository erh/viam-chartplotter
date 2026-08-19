import 'package:latlong2/latlong.dart';

import '../map/measure.dart' show bearingDeg;
import 'route_store.dart';

/// Derived overlay stats for the active route (E5) — a port of the web's
/// `routeStats` (src/marineMap.svelte:573-611): Next = boat to the first
/// waypoint, Final = that plus every remaining leg. Minutes are null (shown
/// blank, never zero) when the boat is effectively stationary (≤ 0.1 kn).
class RouteStats {
  const RouteStats({
    required this.nextNm,
    required this.nextBearingDeg,
    required this.nextMinutes,
    required this.finalNm,
    required this.finalMinutes,
    required this.waypointCount,
  });

  final double nextNm;
  final double nextBearingDeg;
  final double? nextMinutes;
  final double finalNm;
  final double? finalMinutes;
  final int waypointCount;
}

const Distance _dist = Distance();

bool _validCoord(LatLng p) =>
    !(p.latitude == 0 && p.longitude == 0) &&
    p.latitude.abs() <= 90 &&
    p.longitude.abs() <= 180;

RouteStats? computeRouteStats(
    LatLng? boat, List<NavWaypoint> waypoints, double? speedKn) {
  if (boat == null || waypoints.isEmpty || !_validCoord(boat)) return null;
  final spd = speedKn ?? 0;

  final next = waypoints.first.pos;
  final nextMeters = _dist.distance(boat, next);
  final nextNm = nextMeters / 1852;
  // hours = nm / kn; → minutes. Below steerage way SOG is GPS noise, so the
  // estimate would be garbage — blank it instead (web returns Infinity and
  // formats it as "—").
  final double? nextMin = spd > 0.1 ? (nextNm / spd) * 60 : null;

  var totalMeters = nextMeters;
  var prev = next;
  for (var i = 1; i < waypoints.length; i++) {
    totalMeters += _dist.distance(prev, waypoints[i].pos);
    prev = waypoints[i].pos;
  }
  final totalNm = totalMeters / 1852;
  final double? totalMin = spd > 0.1 ? (totalNm / spd) * 60 : null;

  return RouteStats(
    nextNm: nextNm,
    nextBearingDeg: bearingDeg(boat, next),
    nextMinutes: nextMin,
    finalNm: totalNm,
    finalMinutes: totalMin,
    waypointCount: waypoints.length,
  );
}

/// "45 min", "2h 05m" → web formatDurationMin; null/absurd → "—".
String formatDurationMin(double? min) {
  if (min == null || !min.isFinite || min <= 0) return '—';
  if (min < 60) return '${min.round()} min';
  final h = min ~/ 60;
  final m = (min % 60).round();
  return m == 0 ? '${h}h' : '${h}h ${m}m';
}

/// Wall-clock arrival "14:05" → web formatEta; blank when unknowable.
String formatEta(double? min, {DateTime? now}) {
  if (min == null || !min.isFinite || min <= 0) return '—';
  final eta =
      (now ?? DateTime.now()).add(Duration(seconds: (min * 60).round()));
  final hh = eta.hour.toString().padLeft(2, '0');
  final mm = eta.minute.toString().padLeft(2, '0');
  return '$hh:$mm';
}
