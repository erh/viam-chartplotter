import 'dart:convert';

import 'package:http/http.dart' as http;
import 'package:latlong2/latlong.dart';

/// Auto-routing client, ported from src/lib/autoRoute.ts (tests translated
/// alongside). The planner lives on the chart server (Go: render/autoroute.go)
/// because that is where the ENC depth data is; the phone hands it two points
/// and gets back a waypoint list.
///
/// The safe depth is the hard constraint — the route never crosses water
/// charted shoaler than it — and the ideal depth is the soft one: among safe
/// routes, prefer the one that stays that deep. Leave either null and the
/// server uses the boat's configured draft.

const double _metresPerNm = 1852;
const double _metresToFeet = 3.28084;

double metresToFeet(double m) => m * _metresToFeet;
double metresToNm(double m) => m / _metresPerNm;

class AutoRouteResult {
  const AutoRouteResult({
    required this.waypoints,
    required this.distanceMeters,
    required this.directMeters,
    required this.minDepthMeters,
    required this.crossedUnknown,
    required this.safeDepthMeters,
    required this.idealDepthMeters,
    required this.snappedStart,
    required this.snappedEnd,
    required this.cellSizeMeters,
    required this.sections,
    this.warnings = const [],
  });

  final List<LatLng> waypoints;
  final double distanceMeters;
  final double directMeters;

  /// Shoalest charted depth on the route; null when none of it was charted.
  final double? minDepthMeters;
  final bool crossedUnknown;
  final double safeDepthMeters;
  final double idealDepthMeters;
  final bool snappedStart;
  final bool snappedEnd;
  final double cellSizeMeters;

  /// How many independently planned runs the route was split into. More than
  /// 1 means it was too long to plan whole at a useful resolution, and
  /// [cellSizeMeters] is the coarsest any section used.
  final int sections;
  final List<String> warnings;

  static AutoRouteResult fromJson(Map<String, dynamic> j) => AutoRouteResult(
        waypoints: [
          if (j['waypoints'] is List)
            for (final w in j['waypoints'] as List)
              if (w is Map && w['lat'] is num && w['lng'] is num)
                LatLng((w['lat'] as num).toDouble(),
                    (w['lng'] as num).toDouble()),
        ],
        distanceMeters: (j['distance_meters'] as num?)?.toDouble() ?? 0,
        directMeters: (j['direct_meters'] as num?)?.toDouble() ?? 0,
        minDepthMeters: (j['min_depth_meters'] as num?)?.toDouble(),
        crossedUnknown: j['crossed_unknown'] == true,
        safeDepthMeters: (j['safe_depth_meters'] as num?)?.toDouble() ?? 0,
        idealDepthMeters: (j['ideal_depth_meters'] as num?)?.toDouble() ?? 0,
        snappedStart: j['snapped_start'] == true,
        snappedEnd: j['snapped_end'] == true,
        cellSizeMeters: (j['cell_size_meters'] as num?)?.toDouble() ?? 0,
        sections: (j['sections'] as num?)?.toInt() ?? 1,
        warnings: [
          if (j['warnings'] is List)
            for (final w in j['warnings'] as List) w.toString(),
        ],
      );
}

/// Builds the /noaa-enc/autoroute query. Exposed for testing. Only sends
/// what the operator actually chose — an omitted parameter means "use the
/// boat's configured value", which is not the same as sending 0.
String autoRouteUrl(
  String base, {
  required LatLng start,
  required LatLng end,
  double? safeDepthFt,
  double? idealDepthFt,
  double? clearanceM,
  List<String> avoid = const [],
  int? maxWaypoints,
}) {
  final p = <String, String>{
    'startLat': '${start.latitude}',
    'startLon': '${start.longitude}',
    'endLat': '${end.latitude}',
    'endLon': '${end.longitude}',
  };
  if (safeDepthFt != null && safeDepthFt > 0) p['sd'] = '$safeDepthFt';
  if (idealDepthFt != null && idealDepthFt > 0) p['ideal'] = '$idealDepthFt';
  if (clearanceM != null && clearanceM >= 0) p['clearance'] = '$clearanceM';
  if (maxWaypoints != null && maxWaypoints > 1) {
    p['max_waypoints'] = '$maxWaypoints';
  }
  if (avoid.isNotEmpty) p['avoid'] = avoid.join(',');
  return '$base/noaa-enc/autoroute?${Uri(queryParameters: p).query}';
}

/// Plans a route between two points. Throws with the server's own message on
/// failure — "no safe route found …" is the common one and is worth showing
/// verbatim, since it names the depth that made it impossible.
Future<AutoRouteResult> planAutoRoute(
  String base, {
  required LatLng start,
  required LatLng end,
  double? safeDepthFt,
  double? idealDepthFt,
  double? clearanceM,
  List<String> avoid = const [],
  int? maxWaypoints,
  http.Client? client,
}) async {
  final url = Uri.parse(autoRouteUrl(
    base,
    start: start,
    end: end,
    safeDepthFt: safeDepthFt,
    idealDepthFt: idealDepthFt,
    clearanceM: clearanceM,
    avoid: avoid,
    maxWaypoints: maxWaypoints,
  ));
  final resp = await (client?.get(url) ?? http.get(url));
  if (resp.statusCode != 200) {
    var msg = 'auto-route failed (${resp.statusCode})';
    try {
      final body = jsonDecode(resp.body);
      if (body is Map && body['error'] is String) msg = body['error'] as String;
    } catch (_) {
      // non-JSON error body; keep the status message
    }
    throw Exception(msg);
  }
  final body = jsonDecode(resp.body);
  if (body is! Map<String, dynamic>) {
    throw Exception('auto-route returned an unexpected response');
  }
  return AutoRouteResult.fromJson(body);
}

/// The human-readable cautions to show beside a planned route: the server's
/// own warnings plus the ones derived from the result that a skipper should
/// see before loading it.
List<String> routeCautions(AutoRouteResult res) {
  final out = [...res.warnings];
  if (res.crossedUnknown) {
    out.add('part of this route crosses water with no charted depth');
  }
  if (res.sections > 1) {
    out.add('too long to plan in one piece — split into ${res.sections} '
        'sections, coarsest grid ${res.cellSizeMeters.round()} m');
  }
  final minDepth = res.minDepthMeters;
  if (minDepth != null && minDepth.isFinite) {
    out.add('shoalest charted depth on the route: '
        '${metresToFeet(minDepth).toStringAsFixed(1)} ft');
  }
  return out;
}
