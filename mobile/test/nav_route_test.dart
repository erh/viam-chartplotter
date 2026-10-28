import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/protos/common/common.dart' as common_pb;
import 'package:viam_sdk/protos/service/navigation.dart' as nav_pb;
import 'package:viam_chartplotter_mobile/routes/nav_route.dart';
import 'package:viam_chartplotter_mobile/routes/route_store.dart';

nav_pb.Position pos(double lat, double lng) => nav_pb.Position(
    geoPose: common_pb.GeoPose(
        location: common_pb.GeoPoint(latitude: lat, longitude: lng),
        heading: 0));

nav_pb.Status routeStatus(
  List<List<double>> pts, {
  int index = 0,
  nav_pb.GoalState state = nav_pb.GoalState.GOAL_STATE_ACTIVE,
}) =>
    nav_pb.Status(
      goal: nav_pb.GoalStatus(
        state: state,
        mode: nav_pb.NavMode.NAV_MODE_ROUTE,
        target: pos(pts[index.clamp(0, pts.length - 1)][0],
            pts[index.clamp(0, pts.length - 1)][1]),
        route: nav_pb.RouteProgress(
          waypoints: [
            for (final p in pts) nav_pb.Waypoint(position: pos(p[0], p[1]))
          ],
          index: index,
        ),
      ),
    );

NavWaypoint wp(String id, double lat, double lng) =>
    NavWaypoint(id: id, pos: LatLng(lat, lng));

void main() {
  group('navRouteFromStatus', () {
    test('no goal is no route', () {
      final r = navRouteFromStatus(nav_pb.Status());
      expect(r.reached, isEmpty);
      expect(r.remaining, isEmpty);
    });

    test('splits a route at its cursor with absolute-index ids', () {
      final r = navRouteFromStatus(routeStatus([
        [1, 1],
        [2, 2],
        [3, 3],
      ], index: 1));
      expect(r.reached, [const LatLng(1, 1)]);
      expect(r.remaining.map((w) => w.id), ['1', '2']);
      expect(r.remaining.last.pos, const LatLng(3, 3));
    });

    test('a suspended route still counts', () {
      final r = navRouteFromStatus(routeStatus([
        [1, 1],
      ], state: nav_pb.GoalState.GOAL_STATE_SUSPENDED));
      expect(r.remaining, hasLength(1));
      expect(r.suspended, isTrue);
    });

    test('distance remaining comes from progress while active', () {
      final status = routeStatus([
        [1, 1],
        [2, 2],
      ]);
      expect(navRouteFromStatus(status).distanceRemainingM, isNull);
      status.goal.progress = nav_pb.Progress(distanceRemainingM: 1852);
      final r = navRouteFromStatus(status);
      expect(r.distanceRemainingM, 1852);
      expect(r.suspended, isFalse);
    });

    test('a suspended goal reports no distance remaining', () {
      final status = routeStatus([
        [1, 1],
      ], state: nav_pb.GoalState.GOAL_STATE_SUSPENDED);
      status.goal.progress = nav_pb.Progress(distanceRemainingM: 500);
      expect(navRouteFromStatus(status).distanceRemainingM, isNull);
    });

    for (final s in [
      nav_pb.GoalState.GOAL_STATE_SUCCEEDED,
      nav_pb.GoalState.GOAL_STATE_CANCELED,
      nav_pb.GoalState.GOAL_STATE_FAILED,
    ]) {
      test('a $s goal is no route', () {
        final r = navRouteFromStatus(routeStatus([
          [1, 1],
        ], state: s));
        expect(r.remaining, isEmpty);
        expect(r.reached, isEmpty);
      });
    }

    test('a single-target goal is its target, id 0', () {
      final r = navRouteFromStatus(nav_pb.Status(
        goal: nav_pb.GoalStatus(
          state: nav_pb.GoalState.GOAL_STATE_ACTIVE,
          mode: nav_pb.NavMode.NAV_MODE_DIRECT,
          target: pos(4, 5),
        ),
      ));
      expect(r.reached, isEmpty);
      expect(r.remaining.single.id, '0');
      expect(r.remaining.single.pos, const LatLng(4, 5));
    });

    test('a finished route (index past the end) has nothing remaining', () {
      final r = navRouteFromStatus(routeStatus([
        [1, 1],
        [2, 2],
      ], index: 2));
      expect(r.reached, hasLength(2));
      expect(r.remaining, isEmpty);
    });
  });

  group('navRouteRequest', () {
    test('re-sends the reached prefix and starts after it', () {
      final req = navRouteRequest(
          [const LatLng(1, 1)], [wp('1', 2, 2), wp('pending-9', 3, 3)]);
      expect(req.waypoints,
          [const LatLng(1, 1), const LatLng(2, 2), const LatLng(3, 3)]);
      expect(req.startIndex, 1);
      expect(req.isCancel, isFalse);
    });

    test('nothing left to sail is a cancel', () {
      final req = navRouteRequest([const LatLng(1, 1)], const []);
      expect(req.isCancel, isTrue);
    });

    test('an edit round-trips through the status shape', () {
      final status = routeStatus([
        [1, 1],
        [2, 2],
        [3, 3],
      ], index: 1);
      final route = navRouteFromStatus(status);
      final moved = waypointsWithMoved(route.remaining, '2', const LatLng(9, 9));
      final req = navRouteRequest(route.reached, moved);
      expect(req.waypoints.last, const LatLng(9, 9));
      expect(req.startIndex, 1);
    });
  });

  test('navWaypointProto always carries a heading', () {
    final w = navWaypointProto(const LatLng(41, -71));
    expect(w.position.geoPose.hasHeading(), isTrue);
    expect(w.position.geoPose.heading, 0);
    expect(w.position.geoPose.location.latitude, 41);
  });
}
