import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'nav_route.dart';
import 'route_store.dart';

/// Thin wrapper over the SDK's NavigationClient for the machine's nav
/// service (E1/E2). The active route is the service's goal: read with
/// GetNavigationStatus, set with NavigateRoute, paused with Suspend/Resume,
/// cleared with Cancel (see nav_route.dart). The saved-route library uses
/// the Route RPCs plus the routes_meta DoCommand (see route_store.dart).
class NavApi implements RoutesApi {
  NavApi(this._client);

  final NavigationClient _client;

  String get name => _client.name;

  static NavApi fromRobot(RobotClient robot, String name) =>
      NavApi(NavigationClient.fromRobot(robot, name));

  Future<NavRoute> getRoute() async =>
      navRouteFromStatus(await _client.getNavigationStatus());

  /// Make [req] the active route, or cancel when it leaves nothing to sail.
  Future<void> setRoute(NavRouteRequest req) async {
    if (req.isCancel) {
      await _client.cancel();
      return;
    }
    await _client.navigateRoute(
      waypoints: [for (final p in req.waypoints) navWaypointProto(p)],
      startIndex: req.startIndex,
    );
  }

  /// Start a saved route by its id (the service accepts a route's id or
  /// name; the id is unambiguous).
  Future<void> navigateSavedRoute(String routeId) async {
    await _client.navigateRoute(routeName: routeId);
  }

  /// Start an inline route from its first waypoint.
  Future<void> navigateWaypoints(List<LatLng> waypoints) =>
      setRoute(navRouteRequest(const [], [
        for (var i = 0; i < waypoints.length; i++)
          NavWaypoint(id: '$i', pos: waypoints[i])
      ]));

  Future<void> cancel() async {
    await _client.cancel();
  }

  /// Pause the goal; returns the route as the service now reports it.
  Future<NavRoute> suspend() async =>
      navRouteFromStatus(await _client.suspend(reason: 'paused from app'));

  /// Resume a paused goal; returns the route as the service now reports it.
  Future<NavRoute> resume() async =>
      navRouteFromStatus(await _client.resume());

  @override
  Future<List<ApiRoute>> listApiRoutes() async => [
        for (final r in await _client.listRoutes())
          (
            name: r.name,
            waypoints: [
              for (final w in r.waypoints)
                if (waypointLatLng(w) case final p?) p
            ],
          )
      ];

  @override
  Future<void> addRoute(
      String name, List<LatLng> waypoints, Map<String, dynamic> extra) async {
    await _client.addRoute(
        name, [for (final p in waypoints) navWaypointProto(p)],
        extra: extra);
  }

  @override
  Future<void> updateRoute(String id,
      {String? newName,
      List<LatLng>? waypoints,
      Map<String, dynamic>? extra}) async {
    await _client.updateRoute(
      id,
      newName: newName,
      waypoints: waypoints == null
          ? null
          : [for (final p in waypoints) navWaypointProto(p)],
      extra: extra == null || extra.isEmpty ? null : extra,
    );
  }

  @override
  Future<void> deleteRoute(String id) => _client.deleteRoute(id);

  @override
  Future<Map<String, dynamic>> doCommand(Map<String, dynamic> cmd) =>
      _client.doCommand(cmd);
}
