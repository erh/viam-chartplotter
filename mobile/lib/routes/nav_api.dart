import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/viam_sdk.dart';
// The Dart SDK ships no high-level NavigationClient (a flagged beta gap), so
// this wraps the generated gRPC stubs directly, borrowing the channel from
// any existing resource client (they all expose one via ResourceRPCClient).
// ignore: implementation_imports
import 'package:viam_sdk/src/gen/common/v1/common.pb.dart' as common_pb;
// ignore: implementation_imports
import 'package:viam_sdk/src/gen/service/navigation/v1/navigation.pb.dart'
    as nav_pb;
// ignore: implementation_imports
import 'package:viam_sdk/src/gen/service/navigation/v1/navigation.pbgrpc.dart'
    as nav_grpc;
// ignore: implementation_imports
import 'package:viam_sdk/src/utils.dart' show MapStructUtils, StructUtils;

import 'route_store.dart';

/// Thin client for the machine's navigation service (E1/E2): waypoint CRUD
/// via the typed RPCs, everything else via DoCommand verbs. DoCommand goes
/// through toStruct() — the web app has a scar where a plain object
/// serialised to an EMPTY command (App.svelte:2329); the Struct conversion
/// here is explicit for the same reason.
class NavApi implements RoutesApi {
  NavApi(this._client, this.name);

  final nav_grpc.NavigationServiceClient _client;
  final String name;

  /// The nav service named [name] on the robot, borrowing [channelDonor]'s
  /// channel (any resource client on the same robot works).
  static NavApi? fromRobot(RobotClient robot, String name,
      {required Resource channelDonor}) {
    if (channelDonor is! ResourceRPCClient) return null;
    final channel = (channelDonor as ResourceRPCClient).channel;
    return NavApi(nav_grpc.NavigationServiceClient(channel), name);
  }

  Future<List<NavWaypoint>> getWaypoints() async {
    final resp = await _client
        .getWaypoints(nav_pb.GetWaypointsRequest()..name = name);
    return [
      for (final w in resp.waypoints)
        NavWaypoint(
          id: w.id,
          pos: LatLng(w.location.latitude, w.location.longitude),
        ),
    ];
  }

  Future<void> addWaypoint(double lat, double lng) async {
    await _client.addWaypoint(nav_pb.AddWaypointRequest()
      ..name = name
      ..location = (common_pb.GeoPoint()
        ..latitude = lat
        ..longitude = lng));
  }

  Future<void> removeWaypoint(String id) async {
    await _client.removeWaypoint(nav_pb.RemoveWaypointRequest()
      ..name = name
      ..id = id);
  }

  @override
  Future<Map<String, dynamic>> doCommand(Map<String, dynamic> cmd) async {
    final request = common_pb.DoCommandRequest()
      ..name = name
      ..command = cmd.toStruct();
    final response = await _client.doCommand(request);
    return response.result.toMap();
  }

  Future<void> insertWaypoint(String beforeId, double lat, double lng) =>
      doCommand({
        'insert_waypoint': {'before_id': beforeId, 'lat': lat, 'lng': lng}
      });

  Future<void> moveWaypoint(String id, double lat, double lng) => doCommand({
        'move_waypoint': {'id': id, 'lat': lat, 'lng': lng}
      });

  /// One atomic replace — how a saved route gets loaded (E2). Callers must
  /// refetch afterwards so local ids become real backend ObjectIDs.
  Future<void> setWaypoints(List<LatLng> waypoints) => doCommand({
        'set_waypoints': {
          'waypoints': [
            for (final w in waypoints) {'lat': w.latitude, 'lng': w.longitude}
          ]
        }
      });
}
