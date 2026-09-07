import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/chart/chart_search.dart';

// Translated from src/lib/chartSearch.test.ts (web parity).

SearchHit hit({
  String name = 'Brenton Reef Light',
  List<double> bbox = const [-71.4, 41.4, -71.4, 41.4],
  double distanceMeters = 1852,
}) =>
    SearchHit(
      name: name,
      class_: 'LIGHTS',
      label: 'Light',
      cell: 'US5RI1BB',
      lat: 41.4,
      lng: -71.4,
      bbox: bbox,
      distanceMeters: distanceMeters,
    );

void main() {
  group('searchUrl', () {
    test('sends the query alone by default', () {
      final url = Uri.parse(searchUrl('https://charts.example', 'brenton'));
      expect(url.path, '/noaa-enc/search');
      expect(url.queryParameters['q'], 'brenton');
      expect(url.queryParameters.containsKey('lat'), isFalse);
    });

    test('adds the origin so results come back nearest-first', () {
      final url = Uri.parse(
          'http://x${searchUrl('', 'north channel', origin: const LatLng(41.5, -71.3))}');
      expect(double.parse(url.queryParameters['lat']!), 41.5);
      expect(double.parse(url.queryParameters['lon']!), -71.3);
    });

    test('passes limit and class through', () {
      final url = Uri.parse(
          'http://x${searchUrl('', 'reef', limit: 5, objectClass: 'LIGHTS')}');
      expect(url.queryParameters['limit'], '5');
      expect(url.queryParameters['class'], 'LIGHTS');
    });

    test('encodes queries with spaces and punctuation', () {
      final url = Uri.parse('http://x${searchUrl('', 'point judith (n)')}');
      expect(url.queryParameters['q'], 'point judith (n)');
    });
  });

  group('SearchHit.fromJson', () {
    test('carries the area through, defaulting empty when absent', () {
      final withArea = SearchHit.fromJson({
        'name': 'Brenton Reef Light',
        'class': 'LIGHTS',
        'label': 'Light',
        'cell': 'US5RI1BB',
        'lat': 41.4,
        'lng': -71.4,
        'bbox': [-71.4, 41.4, -71.4, 41.4],
        'distance_meters': 1852,
        'area': 'Newport, RI',
      });
      expect(withArea?.area, 'Newport, RI');
      final without =
          SearchHit.fromJson({'name': 'x', 'lat': 41.0, 'lng': -71.0});
      expect(without?.area, '');
    });
  });

  group('framingFor', () {
    test('centres on a point feature rather than fitting a zero extent', () {
      expect(framingFor(hit()), (fit: false, zoom: 15.0));
    });

    test('fits an area feature to its extent', () {
      expect(framingFor(hit(bbox: [-71.6, 41.3, -71.2, 41.6])).fit, isTrue);
    });
  });

  group('formatSearchDistance', () {
    test('gives a decimal for close things and a whole number for far ones',
        () {
      expect(formatSearchDistance(1852), '1.0 nm');
      expect(formatSearchDistance(1852.0 * 42), '42 nm');
    });

    test('is empty when the distance is unknown', () {
      expect(formatSearchDistance(-1), '');
    });
  });

  group('SearchRunner', () {
    test('does not query for a query shorter than the minimum', () async {
      var runs = 0;
      final runner = SearchRunner<List<SearchHit>>((q) async {
        runs++;
        return [hit()];
      }, const [], delay: Duration.zero);
      final results = <(List<SearchHit>, String)>[];
      final short = 'brenton'.substring(0, minQueryLength - 1);
      runner.search(short, (r, q) => results.add((r, q)), (_) {});
      await Future<void>.delayed(const Duration(milliseconds: 20));
      expect(runs, 0);
      expect(results, [(const <SearchHit>[], short)]);
    });

    test('debounces to a single query for a burst of keystrokes', () async {
      final queries = <String>[];
      final runner = SearchRunner<List<SearchHit>>((q) async {
        queries.add(q);
        return [hit()];
      }, const [], delay: const Duration(milliseconds: 30));
      for (final q in ['bre', 'bren', 'brent', 'brenton']) {
        runner.search(q, (_, __) {}, (_) {});
      }
      await Future<void>.delayed(const Duration(milliseconds: 100));
      expect(queries, ['brenton']);
    });

    test("drops a slow earlier response so it can't overwrite a newer one",
        () async {
      final slow = Completer<List<SearchHit>>();
      var call = 0;
      final runner = SearchRunner<List<SearchHit>>((q) {
        call++;
        return call == 1 ? slow.future : Future.value([hit(name: 'FAST')]);
      }, const [], delay: Duration.zero);
      final names = <String?>[];
      runner.search('first', (r, _) => names.add(r.firstOrNull?.name), (_) {});
      await Future<void>.delayed(const Duration(milliseconds: 10));
      runner.search('second', (r, _) => names.add(r.firstOrNull?.name), (_) {});
      await Future<void>.delayed(const Duration(milliseconds: 10));
      // First query resolves after the second one has already been answered.
      slow.complete([hit(name: 'SLOW')]);
      await Future<void>.delayed(const Duration(milliseconds: 10));
      expect(names, contains('FAST'));
      expect(names, isNot(contains('SLOW')));
    });

    test('cancel stops a pending search from firing', () async {
      var runs = 0;
      final runner = SearchRunner<List<SearchHit>>((q) async {
        runs++;
        return [hit()];
      }, const [], delay: const Duration(milliseconds: 30));
      runner.search('brenton', (_, __) {}, (_) {});
      runner.cancel();
      await Future<void>.delayed(const Duration(milliseconds: 100));
      expect(runs, 0);
    });
  });
}
