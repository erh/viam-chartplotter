import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/routes/route_store.dart';

// Optimistic waypoint-edit transforms (E1). These are what the UI applies the
// instant the user acts, before the backend answers — order and pending-id
// semantics here are what the acceptance criteria hang on.

NavWaypoint wp(String id, double lat, double lng) =>
    NavWaypoint(id: id, pos: LatLng(lat, lng));

void main() {
  final chain = [wp('a', 41.0, -71.0), wp('b', 41.1, -71.1), wp('c', 41.2, -71.2)];

  group('pendingWaypointId', () {
    test('has the pending- prefix NavWaypoint.isPending keys on', () {
      final id = pendingWaypointId(DateTime.fromMillisecondsSinceEpoch(1234));
      expect(id, 'pending-1234');
      expect(NavWaypoint(id: id, pos: const LatLng(0, 0)).isPending, isTrue);
      expect(wp('a', 0, 0).isPending, isFalse);
    });
  });

  group('waypointsWithAdded', () {
    test('appends without mutating the original', () {
      final out = waypointsWithAdded(chain, wp('pending-1', 42, -70));
      expect(out.map((w) => w.id), ['a', 'b', 'c', 'pending-1']);
      expect(chain, hasLength(3));
    });
  });

  group('waypointsWithMoved', () {
    test('moves only the matching id, preserving order', () {
      final out = waypointsWithMoved(chain, 'b', const LatLng(45, -60));
      expect(out.map((w) => w.id), ['a', 'b', 'c']);
      expect(out[1].pos, const LatLng(45, -60));
      expect(out[0].pos, chain[0].pos);
    });
    test('is a no-op for an unknown id', () {
      final out = waypointsWithMoved(chain, 'nope', const LatLng(45, -60));
      expect([for (final w in out) w.pos], [for (final w in chain) w.pos]);
    });
  });

  group('waypointsWithInsertedBefore', () {
    test('inserts ahead of the target', () {
      final out =
          waypointsWithInsertedBefore(chain, 'b', wp('pending-2', 41.05, -71.05));
      expect(out.map((w) => w.id), ['a', 'pending-2', 'b', 'c']);
    });
    test('inserting before the first makes it the new head', () {
      final out =
          waypointsWithInsertedBefore(chain, 'a', wp('pending-3', 40.9, -70.9));
      expect(out.first.id, 'pending-3');
    });
    test('degrades to append when the target vanished', () {
      final out =
          waypointsWithInsertedBefore(chain, 'gone', wp('pending-4', 0, 0));
      expect(out.last.id, 'pending-4');
      expect(out, hasLength(4));
    });
  });

  group('waypointsWithRemoved', () {
    test('drops the matching id', () {
      expect(waypointsWithRemoved(chain, 'b').map((w) => w.id), ['a', 'c']);
    });
    test('is a no-op for an unknown id', () {
      expect(waypointsWithRemoved(chain, 'nope'), hasLength(3));
    });
  });
}
