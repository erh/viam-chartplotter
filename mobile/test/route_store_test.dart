import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/routes/route_store.dart';

/// routeStore.test.ts translated. Records every DoCommand the wrapper issues
/// and returns a canned response — the heavy read-modify-write/guard logic
/// lives and is tested in Go (nav_routes_test.go); here we only verify the
/// client shapes the commands right and maps responses back.
class FakeApi implements RoutesApi {
  final calls = <Map<String, dynamic>>[];
  Map<String, dynamic> response = {};

  @override
  Future<Map<String, dynamic>> doCommand(Map<String, dynamic> cmd) async {
    calls.add(cmd);
    return response;
  }

  Map<String, dynamic> get last => calls.last;
}

Route mkRoute({
  String id = 'rte_a',
  String? color = '#ff8800',
  List<LatLng>? waypoints,
  String? scope,
}) {
  const now = '2026-06-19T00:00:00Z';
  return Route(
    id: id,
    name: 'Test',
    source: 'manual',
    color: color,
    createdAt: now,
    updatedAt: now,
    waypoints: waypoints ??
        const [LatLng(41.1, -71.5), LatLng(41.2, -71.4)],
    scope: scope,
  );
}

void main() {
  group('newRouteId', () {
    test('has the rte_ prefix and is unique across calls', () {
      final a = newRouteId();
      final b = newRouteId();
      expect(a, matches(r'^rte_[a-z0-9]+_[0-9a-f]{4}$'));
      expect(a, isNot(equals(b)));
    });
  });

  group('nextColor', () {
    test('returns an unused palette color', () {
      expect(nextColor([mkRoute(color: '#ff8800')]), isNot('#ff8800'));
    });
    test('falls back to a cycled color when the palette is exhausted', () {
      final many = [
        for (var i = 0; i < 20; i++) mkRoute(id: 'r$i', color: '#$i')
      ];
      expect(nextColor(many), isA<String>());
    });
  });

  group('sizeWarning', () {
    test('is false for a small set and true for a large one', () {
      expect(sizeWarning([mkRoute()]), isFalse);
      final big = [
        for (var i = 0; i < 50; i++)
          mkRoute(
            id: 'r$i',
            waypoints: [
              for (var j = 0; j < 2000; j++)
                const LatLng(41.123456, -71.123456)
            ],
          )
      ];
      expect(sizeWarning(big), isTrue);
    });
  });

  group('listRoutes', () {
    test('issues routes_list and returns the routes', () async {
      final api = FakeApi()
        ..response = {
          'routes': [mkRoute(id: 'x').toJson()]
        };
      final routes = await listRoutes(api);
      expect(api.last, {'routes_list': true});
      expect(routes, hasLength(1));
      expect(routes.first.id, 'x');
    });

    test('defaults to empty when the routes field is missing', () async {
      final api = FakeApi()..response = {};
      expect(await listRoutes(api), isEmpty);
    });

    test('parent scope reads back as read-only', () async {
      final api = FakeApi()
        ..response = {
          'routes': [mkRoute(id: 'p', scope: 'parent').toJson()]
        };
      final routes = await listRoutes(api);
      expect(routes.single.readOnly, isTrue);
    });
  });

  group('saveRoute', () {
    test('issues routes_save with the route', () async {
      final api = FakeApi()..response = {'ok': true, 'scope': 'location'};
      final r = mkRoute();
      await saveRoute(api, r);
      expect(api.last, {
        'routes_save': {'route': r.toJson()}
      });
    });
  });

  group('deleteRoute', () {
    test('issues routes_delete with the id', () async {
      final api = FakeApi();
      await deleteRoute(api, 'rte_a');
      expect(api.last, {
        'routes_delete': {'id': 'rte_a'}
      });
    });
  });

  group('renameRoute', () {
    test('issues routes_rename with fields + updatedAt', () async {
      final api = FakeApi();
      await renameRoute(api, 'rte_a',
          name: 'New', color: '#123456', nowIso: '2026-07-01T00:00:00Z');
      expect(api.last, {
        'routes_rename': {
          'id': 'rte_a',
          'name': 'New',
          'color': '#123456',
          'updatedAt': '2026-07-01T00:00:00Z',
        }
      });
    });
  });
}
