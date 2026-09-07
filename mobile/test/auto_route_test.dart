import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/routes/auto_route.dart';

// Translated from src/lib/autoRoute.test.ts (web parity).

const start = LatLng(41.0, -71.5);
const end = LatLng(41.1, -71.4);

AutoRouteResult result({
  double? minDepthMeters = 3.048, // 10 ft
  bool crossedUnknown = false,
  double cellSizeMeters = 25,
  int sections = 1,
  List<String> warnings = const [],
}) =>
    AutoRouteResult(
      waypoints: const [start, end],
      distanceMeters: 10000,
      directMeters: 9000,
      minDepthMeters: minDepthMeters,
      crossedUnknown: crossedUnknown,
      safeDepthMeters: 1.8,
      idealDepthMeters: 3.6,
      snappedStart: false,
      snappedEnd: false,
      cellSizeMeters: cellSizeMeters,
      sections: sections,
      warnings: warnings,
    );

void main() {
  group('autoRouteUrl', () {
    test('sends the endpoints and nothing else by default', () {
      final url = Uri.parse(
          autoRouteUrl('https://charts.example', start: start, end: end));
      expect(url.path, '/noaa-enc/autoroute');
      expect(double.parse(url.queryParameters['startLat']!), 41.0);
      expect(double.parse(url.queryParameters['startLon']!), -71.5);
      expect(double.parse(url.queryParameters['endLat']!), 41.1);
      expect(double.parse(url.queryParameters['endLon']!), -71.4);
      // Omitted, so the server falls back to the boat's configured draft.
      expect(url.queryParameters.containsKey('sd'), isFalse);
      expect(url.queryParameters.containsKey('ideal'), isFalse);
    });

    test('passes depths in feet and the avoid list', () {
      final url = Uri.parse('http://x${autoRouteUrl('', start: start, end: end,
          safeDepthFt: 7, idealDepthFt: 20, avoid: const ['restricted'])}');
      expect(double.parse(url.queryParameters['sd']!), 7);
      expect(double.parse(url.queryParameters['ideal']!), 20);
      expect(url.queryParameters['avoid'], 'restricted');
    });

    test('drops non-positive depths rather than sending a zero', () {
      final url = Uri.parse('http://x${autoRouteUrl('', start: start, end: end,
          safeDepthFt: 0, idealDepthFt: -1)}');
      expect(url.queryParameters.containsKey('sd'), isFalse);
      expect(url.queryParameters.containsKey('ideal'), isFalse);
    });

    test('keeps a zero clearance, which is a real choice', () {
      final url = Uri.parse(
          'http://x${autoRouteUrl('', start: start, end: end, clearanceM: 0)}');
      expect(double.parse(url.queryParameters['clearance']!), 0);
    });

    test('uses a same-origin path when there is no separate chart server', () {
      expect(autoRouteUrl('', start: start, end: end),
          startsWith('/noaa-enc/autoroute?'));
    });
  });

  group('routeCautions', () {
    test('reports the shoalest charted depth in feet', () {
      expect(routeCautions(result()),
          contains('shoalest charted depth on the route: 10.0 ft'));
    });

    test('flags uncharted water', () {
      final out = routeCautions(result(crossedUnknown: true));
      expect(out.any((c) => c.contains('no charted depth')), isTrue);
    });

    test("passes the server's own warnings through", () {
      final out = routeCautions(result(
          warnings: const ['start moved to the nearest navigable water']));
      expect(out.first, 'start moved to the nearest navigable water');
    });

    test('omits the depth line when nothing was charted', () {
      final out =
          routeCautions(result(minDepthMeters: null, crossedUnknown: true));
      expect(out.any((c) => c.startsWith('shoalest')), isFalse);
    });
  });

  group('routeCautions section warning', () {
    test('says nothing about sections for a route planned in one piece', () {
      expect(
          routeCautions(result()).any((c) => c.contains('sections')), isFalse);
    });

    test('reports a sectioned route and the coarsest grid it used', () {
      final out = routeCautions(result(sections: 4, cellSizeMeters: 116));
      expect(
          out.any(
              (c) => c.contains('split into 4 sections') && c.contains('116 m')),
          isTrue);
    });
  });

  group('metresToNm', () {
    test('converts using the international nautical mile', () {
      expect(metresToNm(1852), closeTo(1, 1e-9));
    });
  });
}
