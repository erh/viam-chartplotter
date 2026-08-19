import 'dart:convert';
import 'dart:math' as math;

import 'package:latlong2/latlong.dart';

/// Saved-routes data layer (E2), ported from src/lib/routeStore.ts (tests
/// translated alongside). Storage lives in the Viam location metadata, but
/// the client never talks to the cloud directly — everything goes through
/// the nav service's routes_* DoCommand verbs (nav_routes.go), which
/// authenticate with the machine's own credentials. Read-modify-write,
/// schema/size guards and stats live on the Go side; this module just
/// shapes the calls plus the id/color/size helpers the UI needs.

/// Minimal nav-service surface: a DoCommand passthrough. Backed by the real
/// nav service in the app; a fake in tests.
abstract class RoutesApi {
  Future<Map<String, dynamic>> doCommand(Map<String, dynamic> cmd);
}

/// One waypoint as the nav service reports it. Lives here (not nav_api.dart)
/// so BoatState and tests can use it without importing the gRPC stubs.
class NavWaypoint {
  const NavWaypoint({required this.id, required this.pos});
  final String id;
  final LatLng pos;

  /// Optimistic entries created locally before the backend assigns a real
  /// ObjectID. Never send a pending id back to the service (web parity).
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
// getWaypoints poll replaces them with the backend's truth (and real ids).

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

/// Stable, unique-ish route id: `rte_<time36>_<4 hex>` (web newRouteId).
String newRouteId({DateTime? now, math.Random? rng}) {
  final time = (now ?? DateTime.now())
      .millisecondsSinceEpoch
      .toRadixString(36);
  final r = rng ?? math.Random.secure();
  final rand = r.nextInt(0xffff).toRadixString(16).padLeft(4, '0');
  return 'rte_${time}_$rand';
}

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

Future<List<Route>> listRoutes(RoutesApi api) async {
  final resp = await api.doCommand({'routes_list': true});
  final routes = resp['routes'];
  if (routes is! List) return const [];
  return [
    for (final r in routes)
      if (Route.fromJson(r) case final route?) route
  ];
}

Future<void> saveRoute(RoutesApi api, Route route) async {
  await api.doCommand({
    'routes_save': {'route': route.toJson()}
  });
}

Future<void> deleteRoute(RoutesApi api, String id) async {
  await api.doCommand({
    'routes_delete': {'id': id}
  });
}

Future<void> renameRoute(
  RoutesApi api,
  String id, {
  String? name,
  String? notes,
  String? color,
  required String nowIso,
}) async {
  await api.doCommand({
    'routes_rename': {
      'id': id,
      if (name != null) 'name': name,
      if (notes != null) 'notes': notes,
      if (color != null) 'color': color,
      'updatedAt': nowIso,
    }
  });
}
