import 'dart:convert';

import 'package:latlong2/latlong.dart';

/// Saved-routes data layer (E2), ported from src/lib/routeStore.ts (tests
/// translated alongside). Storage lives in the Viam location metadata, but
/// the client never talks to the cloud directly: it goes through the nav
/// service, which authenticates with the machine's own credentials. Names
/// and waypoints use the navigation API's Route RPCs; the library fields the
/// API's Route has no slot for (id, notes, color, source, dates, stats,
/// scope) are written via request extras and read back with the one
/// routes_meta DoCommand (nav_routes.go).

/// A route as ListRoutes reports it: name and coordinates only.
typedef ApiRoute = ({String name, List<LatLng> waypoints});

/// The nav service's route calls. Backed by NavApi in the app; a fake in
/// tests. Routes are addressed by id (the service accepts id or name).
abstract class RoutesApi {
  Future<List<ApiRoute>> listApiRoutes();
  Future<void> addRoute(
      String name, List<LatLng> waypoints, Map<String, dynamic> extra);
  Future<void> updateRoute(String id,
      {String? newName,
      List<LatLng>? waypoints,
      Map<String, dynamic>? extra});
  Future<void> deleteRoute(String id);
  Future<Map<String, dynamic>> doCommand(Map<String, dynamic> cmd);
}

/// One waypoint as the nav service reports it. Lives here (not nav_api.dart)
/// so BoatState and tests can use it without importing the gRPC stubs.
class NavWaypoint {
  const NavWaypoint({required this.id, required this.pos});
  final String id;
  final LatLng pos;

  /// Optimistic entries created locally before the service reports the
  /// waypoint back with its index id.
  bool get isPending => id.startsWith('pending-');
}

/// Soft client-side warning threshold; the hard limit is the backend's.
const int sizeWarnBytes = 200 * 1024;

const List<String> routePalette = [
  '#ff8800',
  '#1e90ff',
  '#2ecc71',
  '#e74c3c',
  '#9b59b6',
  '#f1c40f',
  '#16a085',
  '#e84393',
];

class Route {
  const Route({
    required this.id,
    required this.name,
    this.notes,
    this.color,
    required this.source,
    required this.createdAt,
    required this.updatedAt,
    required this.waypoints,
    this.distanceNm,
    this.count,
    this.scope,
  });

  final String id;
  final String name;
  final String? notes;
  final String? color;
  final String source; // "manual" | "track"
  final String createdAt;
  final String updatedAt;
  final List<LatLng> waypoints;
  final double? distanceNm; // stats.distanceNm
  final int? count; // stats.count

  /// "location" = this machine's location; "parent" = inherited from an
  /// ancestor location and READ-ONLY here — the UI must not offer
  /// save/rename/delete. Set by the backend on list responses.
  final String? scope;

  bool get readOnly => scope == 'parent';

  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        if (notes != null) 'notes': notes,
        if (color != null) 'color': color,
        'source': source,
        'createdAt': createdAt,
        'updatedAt': updatedAt,
        'waypoints': [
          for (final w in waypoints) {'lat': w.latitude, 'lng': w.longitude}
        ],
        if (distanceNm != null || count != null)
          'stats': {
            if (distanceNm != null) 'distanceNm': distanceNm,
            if (count != null) 'count': count,
          },
        if (scope != null) 'scope': scope,
      };

  static Route? fromJson(dynamic j) {
    if (j is! Map) return null;
    final id = j['id'];
    if (id is! String || id.isEmpty) return null;
    final wps = <LatLng>[];
    final rawWps = j['waypoints'];
    if (rawWps is List) {
      for (final w in rawWps) {
        if (w is Map && w['lat'] is num && w['lng'] is num) {
          wps.add(LatLng(
              (w['lat'] as num).toDouble(), (w['lng'] as num).toDouble()));
        }
      }
    }
    final stats = j['stats'];
    return Route(
      id: id,
      name: (j['name'] ?? '').toString(),
      notes: j['notes']?.toString(),
      color: j['color']?.toString(),
      source: (j['source'] ?? 'manual').toString(),
      createdAt: (j['createdAt'] ?? '').toString(),
      updatedAt: (j['updatedAt'] ?? '').toString(),
      waypoints: wps,
      distanceNm:
          stats is Map ? (stats['distanceNm'] as num?)?.toDouble() : null,
      count: stats is Map ? (stats['count'] as num?)?.toInt() : null,
      scope: j['scope']?.toString(),
    );
  }
}

// ---- optimistic waypoint edits (E1) ---------------------------------------
// Pure list transforms applied the moment the user acts; the next
// nav-status poll replaces them with the service's truth (and index ids).

/// Local id for a waypoint the backend hasn't acknowledged yet.
String pendingWaypointId(DateTime now) =>
    'pending-${now.millisecondsSinceEpoch}';

List<NavWaypoint> waypointsWithAdded(List<NavWaypoint> wps, NavWaypoint w) =>
    [...wps, w];

List<NavWaypoint> waypointsWithMoved(
        List<NavWaypoint> wps, String id, LatLng pos) =>
    [
      for (final w in wps)
        w.id == id ? NavWaypoint(id: w.id, pos: pos) : w
    ];

List<NavWaypoint> waypointsWithInsertedBefore(
    List<NavWaypoint> wps, String beforeId, NavWaypoint w) {
  final i = wps.indexWhere((x) => x.id == beforeId);
  if (i < 0) return [...wps, w]; // target vanished: degrade to append
  return [...wps.sublist(0, i), w, ...wps.sublist(i)];
}

List<NavWaypoint> waypointsWithRemoved(List<NavWaypoint> wps, String id) =>
    [for (final w in wps) if (w.id != id) w];

/// An unused palette color, cycling once the palette is exhausted.
String nextColor(List<Route> existing) {
  final used = {for (final r in existing) r.color}..remove(null);
  for (final c in routePalette) {
    if (!used.contains(c)) return c;
  }
  return routePalette[existing.length % routePalette.length];
}

/// Warn before a save the backend would reject (web sizeWarning).
bool sizeWarning(List<Route> routes) =>
    utf8.encode(jsonEncode([for (final r in routes) r.toJson()])).length >
    sizeWarnBytes;

/// Pair ListRoutes results with routes_meta entries (web pairRoutes). Both
/// come from the same server-side list, so they normally line up by index;
/// if they don't (a route changed between the two calls), fall back to the
/// first unused entry with the same name. A route with no metadata is
/// dropped: it has no id to edit by.
List<Route> pairRoutes(List<ApiRoute> apiRoutes, List<dynamic> metas) {
  String? metaName(int k) {
    final m = metas[k];
    return m is Map ? m['name']?.toString() : null;
  }

  final used = <int>{};
  final out = <Route>[];
  for (var i = 0; i < apiRoutes.length; i++) {
    final r = apiRoutes[i];
    var j = i < metas.length && metaName(i) == r.name && !used.contains(i)
        ? i
        : -1;
    if (j < 0) {
      for (var k = 0; k < metas.length; k++) {
        if (!used.contains(k) && metaName(k) == r.name) {
          j = k;
          break;
        }
      }
    }
    if (j < 0) continue;
    final meta = Route.fromJson(metas[j]);
    if (meta == null) continue;
    used.add(j);
    out.add(Route(
      id: meta.id,
      name: meta.name,
      notes: meta.notes,
      color: meta.color,
      source: meta.source,
      createdAt: meta.createdAt,
      updatedAt: meta.updatedAt,
      waypoints: r.waypoints,
      distanceNm: meta.distanceNm,
      count: meta.count,
      scope: meta.scope,
    ));
  }
  return out;
}

Future<List<Route>> listRoutes(RoutesApi api) async {
  final results = await Future.wait<Object>([
    api.listApiRoutes(),
    api.doCommand({'routes_meta': true}),
  ]);
  final apiRoutes = results[0] as List<ApiRoute>;
  final metas = (results[1] as Map<String, dynamic>)['routes'];
  return pairRoutes(apiRoutes, metas is List ? metas : const []);
}

/// Save a new route; the service assigns its id and timestamps.
Future<void> createRoute(
  RoutesApi api, {
  required String name,
  required String source,
  String? color,
  String? notes,
  required List<LatLng> waypoints,
}) =>
    api.addRoute(name, waypoints, {
      'source': source,
      if (color != null) 'color': color,
      if (notes != null) 'notes': notes,
    });

/// Overwrite a route's geometry, keeping everything else.
Future<void> replaceRouteWaypoints(
        RoutesApi api, String id, List<LatLng> waypoints) =>
    api.updateRoute(id, waypoints: waypoints);

/// Change a route's name, notes and/or color.
Future<void> updateRouteFields(
  RoutesApi api,
  String id, {
  String? name,
  String? notes,
  String? color,
}) =>
    api.updateRoute(id, newName: name, extra: {
      if (notes != null) 'notes': notes,
      if (color != null) 'color': color,
    });

Future<void> deleteRoute(RoutesApi api, String id) => api.deleteRoute(id);
