import 'package:latlong2/latlong.dart';
// The generated navigation protos (Status, GoalStatus, Waypoint, ...). Only
// the NavigationClient is exported from package:viam_sdk itself.
import 'package:viam_sdk/protos/common/common.dart' as common_pb;
import 'package:viam_sdk/protos/service/navigation.dart' as nav_pb;

import 'route_store.dart';

// The active route as the navigation API models it (see nav_service.go). The
// nav service holds one goal: an ordered waypoint list with a cursor. Points
// before the cursor have been reached and stay on record; from the cursor on
// is what the boat still has to sail. There are no per-waypoint ids or edit
// verbs, so every edit re-sends the whole list with start_index at the
// cursor. Pure: unit-tested in test/nav_route_test.dart.

/// The open goal's waypoints, split at the cursor.
class NavRoute {
  const NavRoute({
    this.reached = const [],
    this.remaining = const [],
    this.suspended = false,
    this.distanceRemainingM,
  });

  /// Already-reached waypoints (`route.waypoints[:index]`), re-sent with
  /// every edit so the trip log survives it.
  final List<LatLng> reached;

  /// Still to sail. Each id is the waypoint's absolute index in the goal's
  /// full list, as a string — stable for as long as the goal is.
  final List<NavWaypoint> remaining;

  /// The goal is paused (Suspend); Resume picks it up again.
  final bool suspended;

  /// The service's distance still to sail, in meters: only while the goal is
  /// active and the service reports progress.
  final double? distanceRemainingM;

  static const NavRoute none = NavRoute();
}

LatLng? _latLng(nav_pb.Position p) {
  if (!p.hasGeoPose() || !p.geoPose.hasLocation()) return null;
  final loc = p.geoPose.location;
  return LatLng(loc.latitude, loc.longitude);
}

/// A waypoint's coordinates, or null for one that only names a saved
/// location.
LatLng? waypointLatLng(nav_pb.Waypoint w) =>
    w.hasPosition() ? _latLng(w.position) : null;

/// The active route in [status]: an ACTIVE or SUSPENDED goal's waypoints. A
/// route goal splits `route.waypoints` at `route.index`; a single-target goal
/// (navigate/go to a point or location) is just its target, id "0". Anything
/// else — no goal, or one that succeeded, failed or was canceled — is none.
NavRoute navRouteFromStatus(nav_pb.Status status) {
  if (!status.hasGoal()) return NavRoute.none;
  final goal = status.goal;
  if (goal.state != nav_pb.GoalState.GOAL_STATE_ACTIVE &&
      goal.state != nav_pb.GoalState.GOAL_STATE_SUSPENDED) {
    return NavRoute.none;
  }
  final suspended = goal.state == nav_pb.GoalState.GOAL_STATE_SUSPENDED;
  final double? distance = !suspended && goal.hasProgress()
      ? goal.progress.distanceRemainingM
      : null;
  if (goal.hasRoute()) {
    final wps = goal.route.waypoints;
    final index = goal.route.index.clamp(0, wps.length);
    final reached = <LatLng>[];
    final remaining = <NavWaypoint>[];
    for (var i = 0; i < wps.length; i++) {
      // A waypoint that only names a saved location carries no coordinates.
      final pos = waypointLatLng(wps[i]);
      if (pos == null) continue;
      if (i < index) {
        reached.add(pos);
      } else {
        remaining.add(NavWaypoint(id: '$i', pos: pos));
      }
    }
    return NavRoute(
        reached: reached,
        remaining: remaining,
        suspended: suspended,
        distanceRemainingM: distance);
  }
  final target = goal.hasTarget() ? _latLng(goal.target) : null;
  if (target == null) return NavRoute.none;
  return NavRoute(
      remaining: [NavWaypoint(id: '0', pos: target)],
      suspended: suspended,
      distanceRemainingM: distance);
}

/// What to send after an edit: NavigateRoute with [waypoints] from
/// [startIndex], or — when nothing is left to sail — Cancel.
class NavRouteRequest {
  const NavRouteRequest(this.waypoints, this.startIndex);
  final List<LatLng> waypoints;
  final int startIndex;

  bool get isCancel => waypoints.length <= startIndex;
}

/// The request that makes [remaining] the route still to sail, keeping the
/// [reached] prefix on record. An empty [remaining] is a cancel.
NavRouteRequest navRouteRequest(
    List<LatLng> reached, List<NavWaypoint> remaining) {
  if (remaining.isEmpty) return const NavRouteRequest([], 0);
  return NavRouteRequest(
      [...reached, for (final w in remaining) w.pos], reached.length);
}

/// A waypoint on the wire. Every GeoPose must carry a heading (the API
/// rejects one with neither heading nor orientation); a waypoint has none.
nav_pb.Waypoint navWaypointProto(LatLng p) => nav_pb.Waypoint(
      position: nav_pb.Position(
        geoPose: common_pb.GeoPose(
          location: common_pb.GeoPoint(
              latitude: p.latitude, longitude: p.longitude),
          heading: 0,
        ),
      ),
    );
