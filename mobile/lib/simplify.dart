import 'dart:math' as math;

import 'package:latlong2/latlong.dart';

/// Pure geometry helpers for turning a recorded track into a route (E3),
/// ported verbatim from src/lib/simplify.ts (tests translated alongside).
///
/// A track is a dense, time-ordered list of fixes; a route is a handful of
/// waypoints. We reduce in two passes (per ROUTES_SPEC.md):
///   1. distance decimation — drop any fix closer than `granularityMeters` to
///      the last kept fix, giving predictable spacing and killing GPS jitter;
///   2. Douglas–Peucker — drop fixes that lie within `toleranceMeters` of the
///      straight line between their neighbours, preserving turns.
/// First and last fixes are always kept. All distances are real meters
/// (haversine for spacing, a local equirectangular projection for the DP
/// perpendicular distance), so the thresholds mean the same thing everywhere.

const double _earthRadiusM = 6371008.8;
const double _deg2rad = math.pi / 180;

class SimplifyOptions {
  const SimplifyOptions({
    required this.granularityMeters,
    this.toleranceMeters,
    this.maxPoints,
  });

  final double granularityMeters;

  /// Defaults to granularityMeters / 2.
  final double? toleranceMeters;

  /// Hard ceiling on output size; if DP still exceeds it, tolerance is raised
  /// and DP re-run until the count fits (capped=true in the result).
  final int? maxPoints;
}

class SimplifiedTrack {
  const SimplifiedTrack({
    required this.waypoints,
    required this.capped,
    required this.inputCount,
  });

  final List<LatLng> waypoints;

  /// True if maxPoints forced extra simplification beyond the requested
  /// tolerance — the UI should surface this so the user can raise granularity.
  final bool capped;
  final int inputCount;
}

/// Great-circle distance in meters.
double haversineMeters(LatLng a, LatLng b) {
  final dLat = (b.latitude - a.latitude) * _deg2rad;
  final dLng = (b.longitude - a.longitude) * _deg2rad;
  final lat1 = a.latitude * _deg2rad;
  final lat2 = b.latitude * _deg2rad;
  final h = math.pow(math.sin(dLat / 2), 2) +
      math.cos(lat1) * math.cos(lat2) * math.pow(math.sin(dLng / 2), 2);
  return 2 * _earthRadiusM * math.asin(math.min(1, math.sqrt(h)));
}

/// Total length of an ordered polyline, in meters.
double pathLengthMeters(List<LatLng> points) {
  var total = 0.0;
  for (var i = 1; i < points.length; i++) {
    total += haversineMeters(points[i - 1], points[i]);
  }
  return total;
}

/// Perpendicular distance (meters) from p to the segment a–b, using a local
/// equirectangular projection centred on `a` (so x is scaled by cos(lat) and
/// the result is in meters). Degenerate segment (a==b) falls back to point
/// distance.
double _perpendicularMeters(LatLng p, LatLng a, LatLng b) {
  final cosLat = math.cos(a.latitude * _deg2rad);
  const ax = 0.0;
  const ay = 0.0;
  final bx = (b.longitude - a.longitude) * _deg2rad * cosLat * _earthRadiusM;
  final by = (b.latitude - a.latitude) * _deg2rad * _earthRadiusM;
  final px = (p.longitude - a.longitude) * _deg2rad * cosLat * _earthRadiusM;
  final py = (p.latitude - a.latitude) * _deg2rad * _earthRadiusM;

  final dx = bx - ax;
  final dy = by - ay;
  final segLenSq = dx * dx + dy * dy;
  if (segLenSq == 0) {
    return math.sqrt((px - ax) * (px - ax) + (py - ay) * (py - ay));
  }
  // Project P onto the (clamped) segment, then measure the gap.
  var t = ((px - ax) * dx + (py - ay) * dy) / segLenSq;
  t = t.clamp(0.0, 1.0);
  final cx = ax + t * dx;
  final cy = ay + t * dy;
  return math.sqrt((px - cx) * (px - cx) + (py - cy) * (py - cy));
}

/// Distance decimation: keep the first fix, then each fix at least
/// granularityMeters from the last kept one. The final fix is always kept so
/// the route ends where the track ends.
List<LatLng> decimateByDistance(List<LatLng> points, double granularityMeters) {
  if (points.length <= 2 || granularityMeters <= 0) {
    return List.of(points);
  }
  final out = <LatLng>[points.first];
  var last = points.first;
  for (var i = 1; i < points.length - 1; i++) {
    if (haversineMeters(points[i], last) >= granularityMeters) {
      out.add(points[i]);
      last = points[i];
    }
  }
  out.add(points.last);
  return out;
}

/// Iterative Douglas–Peucker (explicit stack to avoid deep recursion on long
/// tracks). Returns the kept points in order, always including the endpoints.
List<LatLng> douglasPeucker(List<LatLng> points, double toleranceMeters) {
  final n = points.length;
  if (n <= 2 || toleranceMeters <= 0) {
    return List.of(points);
  }
  final keep = List<bool>.filled(n, false);
  keep[0] = true;
  keep[n - 1] = true;
  final stack = <(int, int)>[(0, n - 1)];
  while (stack.isNotEmpty) {
    final (start, end) = stack.removeLast();
    var maxDist = -1.0;
    var idx = -1;
    for (var i = start + 1; i < end; i++) {
      final d = _perpendicularMeters(points[i], points[start], points[end]);
      if (d > maxDist) {
        maxDist = d;
        idx = i;
      }
    }
    if (maxDist > toleranceMeters && idx != -1) {
      keep[idx] = true;
      stack.add((start, idx));
      stack.add((idx, end));
    }
  }
  return [
    for (var i = 0; i < n; i++)
      if (keep[i]) points[i]
  ];
}

/// Full pipeline: decimate, then Douglas–Peucker, then enforce maxPoints by
/// escalating tolerance. Invalid coordinates (NaN / out of range) are dropped
/// up front; timestamps never enter — a route has no timestamps.
SimplifiedTrack simplifyTrack(List<LatLng> points, SimplifyOptions opts) {
  final cleaned = [
    for (final p in points)
      if (p.latitude.isFinite &&
          p.longitude.isFinite &&
          p.latitude >= -90 &&
          p.latitude <= 90 &&
          p.longitude >= -180 &&
          p.longitude <= 180)
        LatLng(p.latitude, p.longitude)
  ];

  final inputCount = cleaned.length;
  if (inputCount <= 2) {
    return SimplifiedTrack(
        waypoints: cleaned, capped: false, inputCount: inputCount);
  }

  final decimated = decimateByDistance(cleaned, opts.granularityMeters);
  var tolerance =
      opts.toleranceMeters ?? math.max(1, opts.granularityMeters / 2);
  var result = douglasPeucker(decimated, tolerance);

  var capped = false;
  final maxPoints = opts.maxPoints ?? 0;
  if (maxPoints > 0) {
    // Escalate tolerance geometrically until the route fits. Bounded loop:
    // tolerance grows ×1.6 each pass, so it converges in a handful of steps.
    var guard = 0;
    while (result.length > maxPoints && guard < 40) {
      capped = true;
      tolerance *= 1.6;
      result = douglasPeucker(decimated, tolerance);
      guard++;
    }
  }

  return SimplifiedTrack(
      waypoints: result, capped: capped, inputCount: inputCount);
}
