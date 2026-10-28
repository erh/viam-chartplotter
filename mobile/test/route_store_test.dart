import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/routes/route_store.dart';

/// routeStore.test.ts translated. Records every call the wrapper issues and
/// returns canned responses: the heavy read-modify-write/guard logic lives
/// and is tested in Go (nav_routes_test.go); here we only verify the client
/// shapes the calls right and pairs responses back.
class FakeApi implements RoutesApi {
  final calls = <List<Object?>>[];
  List<ApiRoute> apiRoutes = [];
  Map<String, dynamic> response = {};

  @override
  Future<List<ApiRoute>> listApiRoutes() async {
    calls.add(['listRoutes']);
    return apiRoutes;
  }

  @override
  Future<void> addRoute(String name, List<LatLng> waypoints,
      Map<String, dynamic> extra) async {
    calls.add(['addRoute', name, waypoints, extra]);
  }

  @override
  Future<void> updateRoute(String id,
      {String? newName,
      List<LatLng>? waypoints,
      Map<String, dynamic>? extra}) async {
    calls.add(['updateRoute', id, newName, waypoints, extra]);
  }

  @override
  Future<void> deleteRoute(String id) async {
    calls.add(['deleteRoute', id]);
  }

  @override
  Future<Map<String, dynamic>> doCommand(Map<String, dynamic> cmd) async {
    calls.add(['doCommand', cmd]);
    return response;
  }

  List<Object?> get last => calls.last;
}

const _wps = [LatLng(41.1, -71.5), LatLng(41.2, -71.4)];

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
    waypoints: waypoints ?? _wps,
    scope: scope,
  );
}

Map<String, dynamic> meta(String id, String name, {String? scope}) => {
      'id': id,
      'name': name,
      'source': 'manual',
      'createdAt': '2026-06-19T00:00:00Z',
      'updatedAt': '2026-06-19T00:00:00Z',
      'stats': {'distanceNm': 6.2, 'count': 2},
      if (scope != null) 'scope': scope,
    };

ApiRoute apiRoute(String name, [List<LatLng> wps = _wps]) =>
    (name: name, waypoints: wps);

void main() {
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

  group('pairRoutes', () {
    test('pairs by index when the names line up', () {
      final routes = pairRoutes(
          [apiRoute('A'), apiRoute('B')], [meta('a', 'A'), meta('b', 'B')]);
      expect([for (final r in routes) r.id], ['a', 'b']);
      expect(routes.first.waypoints, _wps);
      expect(routes.first.distanceNm, 6.2);
    });

    test('falls back to the first unused name match', () {
      final routes = pairRoutes([apiRoute('B'), apiRoute('A')],
          [meta('a', 'A'), meta('b', 'B')]);
      expect([for (final r in routes) r.id], ['b', 'a']);
    });

    test('keeps duplicate names apart', () {
      final routes = pairRoutes([apiRoute('A'), apiRoute('A')],
          [meta('a1', 'A'), meta('a2', 'A')]);
      expect([for (final r in routes) r.id], ['a1', 'a2']);
    });

    test('drops a route with no metadata', () {
      final routes =
          pairRoutes([apiRoute('A'), apiRoute('New')], [meta('a', 'A')]);
      expect([for (final r in routes) r.id], ['a']);
    });
  });

  group('listRoutes', () {
    test('combines ListRoutes with routes_meta', () async {
      final api = FakeApi()
        ..apiRoutes = [apiRoute('A')]
        ..response = {
          'routes': [meta('a', 'A', scope: 'parent')]
        };
      final routes = await listRoutes(api);
      expect(api.calls, [
        ['listRoutes'],
        [
          'doCommand',
          {'routes_meta': true}
        ],
      ]);
      expect(routes.single.id, 'a');
      expect(routes.single.readOnly, isTrue);
    });

    test('is empty when routes_meta has no routes field', () async {
      final api = FakeApi()..apiRoutes = [apiRoute('A')];
      expect(await listRoutes(api), isEmpty);
    });
  });

  group('createRoute', () {
    test('sends name and waypoints, library fields in extra', () async {
      final api = FakeApi();
      await createRoute(api,
          name: 'N', source: 'track', color: '#123456', waypoints: _wps);
      expect(api.last, [
        'addRoute',
        'N',
        _wps,
        {'source': 'track', 'color': '#123456'}
      ]);
    });
  });

  group('replaceRouteWaypoints', () {
    test('updates only the waypoints, by id', () async {
      final api = FakeApi();
      await replaceRouteWaypoints(api, 'rte_a', _wps);
      expect(api.last, ['updateRoute', 'rte_a', null, _wps, null]);
    });
  });

  group('updateRouteFields', () {
    test('renames via newName, notes/color via extra', () async {
      final api = FakeApi();
      await updateRouteFields(api, 'rte_a', name: 'New', color: '#123456');
      expect(api.last, [
        'updateRoute',
        'rte_a',
        'New',
        null,
        {'color': '#123456'}
      ]);
    });
  });

  group('deleteRoute', () {
    test('deletes by id', () async {
      final api = FakeApi();
      await deleteRoute(api, 'rte_a');
      expect(api.last, ['deleteRoute', 'rte_a']);
    });
  });
}
