import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/isobars.dart';

// F4 — isobar data plumbing and the zoom/label ladder ported from
// src/lib/isobarLayer.ts. The contour geometry itself is server-side
// (weather/noaa_isobars.go marching squares); the client's contract is the
// GeoJSON FeatureCollection tested here.

/// A payload shaped exactly like the server's decodeGFSIsobars output:
/// short 2-point LineString segments per (cell, level) crossing, H/L Point
/// extrema, and the meta block.
const _cannedPayload = '''
{
  "type": "FeatureCollection",
  "features": [
    {"type":"Feature",
     "geometry":{"type":"LineString","coordinates":[[-71.0,41.0],[-70.75,41.1]]},
     "properties":{"hPa":1012}},
    {"type":"Feature",
     "geometry":{"type":"LineString","coordinates":[[-70.75,41.1],[-70.5,41.2]]},
     "properties":{"hPa":1012}},
    {"type":"Feature",
     "geometry":{"type":"LineString","coordinates":[[-71.0,40.5],[-70.75,40.5]]},
     "properties":{"hPa":1014}},
    {"type":"Feature",
     "geometry":{"type":"Point","coordinates":[-69.5,39.0]},
     "properties":{"hPa":1024,"kind":"H"}},
    {"type":"Feature",
     "geometry":{"type":"Point","coordinates":[-75.0,44.0]},
     "properties":{"hPa":996,"kind":"L"}}
  ],
  "meta":{"refTime":"2026-08-18T12:00:00Z","forecastTime":6,"stepHPa":2}
}
''';

void main() {
  group('parseIsobars', () {
    test('parses lines, extrema, and meta from the server shape', () {
      final field = parseIsobars(_cannedPayload);

      expect(field.lines, hasLength(3));
      final first = field.lines.first;
      expect(first.hPa, 1012);
      // GeoJSON is [lon, lat]; LatLng is (lat, lon).
      expect(first.points.first, const LatLng(41.0, -71.0));
      expect(first.points.last, const LatLng(41.1, -70.75));
      expect(first.labelAnchor, first.points.first);

      expect(field.extrema, hasLength(2));
      final high = field.extrema.first;
      expect(high.kind, 'H');
      expect(high.hPa, 1024);
      expect(high.position, const LatLng(39.0, -69.5));
      expect(field.extrema.last.kind, 'L');

      expect(field.refTime, '2026-08-18T12:00:00Z');
      expect(field.forecastTime, 6);
      expect(field.stepHPa, 2);
    });

    test('skips malformed features instead of failing the frame', () {
      final field = parseIsobars('''
      {"type":"FeatureCollection","features":[
        {"type":"Feature","geometry":{"type":"LineString",
          "coordinates":[[0.0,10.0],[0.25,10.0]]},"properties":{"hPa":1008}},
        {"type":"Feature","geometry":{"type":"LineString",
          "coordinates":[[0.0,10.0]]},"properties":{"hPa":1008}},
        {"type":"Feature","geometry":{"type":"LineString",
          "coordinates":[[0.0,10.0],[0.25,10.0]]},"properties":{}},
        {"type":"Feature","geometry":{"type":"Point",
          "coordinates":[1.0,2.0]},"properties":{"hPa":1000,"kind":"X"}},
        {"bogus":true},
        42
      ]}''');
      expect(field.lines, hasLength(1));
      expect(field.extrema, isEmpty);
    });

    test('accepts stitched multi-point LineStrings, not just 2-point segments',
        () {
      // The server currently emits 2-point stubs, but the GeoJSON contract
      // allows longer polylines — a future server-side stitcher must not
      // break the client.
      final field = parseIsobars('''
      {"type":"FeatureCollection","features":[
        {"type":"Feature","geometry":{"type":"LineString",
          "coordinates":[[0.0,10.0],[0.25,10.1],[0.5,10.2],[0.75,10.3]]},
         "properties":{"hPa":1016}}
      ]}''');
      expect(field.lines.single.points, hasLength(4));
    });

    test('missing meta falls back to the 2 hPa default', () {
      final field =
          parseIsobars('{"type":"FeatureCollection","features":[]}');
      expect(field.lines, isEmpty);
      expect(field.extrema, isEmpty);
      expect(field.refTime, isNull);
      expect(field.stepHPa, 2);
    });

    test('throws on a body that is not a FeatureCollection', () {
      expect(() => parseIsobars('[]'), throwsFormatException);
      expect(() => parseIsobars('{"type":"Nope"}'), throwsFormatException);
      expect(() => parseIsobars('{"type":"FeatureCollection"}'),
          throwsFormatException);
    });
  });

  group('fetchIsobars', () {
    test('hits the wind-cache endpoint pattern and decodes the body',
        () async {
      Uri? seen;
      final client = MockClient((req) async {
        seen = req.url;
        return http.Response(_cannedPayload, 200);
      });
      final field = await fetchIsobars('http://boat:8888',
          fh: 24, client: client);
      expect(seen.toString(),
          'http://boat:8888/noaa-weather/data/gfs-isobars/latest.json?fh=24');
      expect(field.lines, hasLength(3));
      expect(field.forecastTime, 6);
    });

    test('throws on a non-200 response', () {
      final client = MockClient((req) async => http.Response('nope', 503));
      expect(fetchIsobars('http://boat:8888', client: client),
          throwsA(isA<http.ClientException>()));
    });

    test('throws on a garbage body', () {
      final client =
          MockClient((req) async => http.Response('not json', 200));
      expect(fetchIsobars('http://boat:8888', client: client),
          throwsA(isA<FormatException>()));
    });
  });

  group('zoom / label ladder (web isobarStyle parity)', () {
    test('half-step 2 hPa lines hide at overview zoom, draw close in', () {
      // 1010 is a 2 hPa in-between level (not divisible by 4).
      expect(isHalfStepHPa(1010), isTrue);
      expect(isHalfStepHPa(1012), isFalse);
      expect(isobarLineVisible(1010, 0.4), isFalse);
      expect(isobarLineVisible(1010, 0.2), isTrue);
      // 4 hPa marine-standard contours always draw.
      expect(isobarLineVisible(1012, 0.4), isTrue);
      expect(isobarLineVisible(1012, 0.2), isTrue);
    });

    test('stroke tiers match the web buildStyle ladder', () {
      expect(isobarTier(1000), IsobarTier.reference);
      expect(isobarTier(1020), IsobarTier.heavy);
      expect(isobarTier(1012), IsobarTier.standard);
      expect(isobarTier(1010), IsobarTier.half);
    });

    IsobarLine lineAt(double lon, double lat, int hPa) => IsobarLine(
        points: [LatLng(lat, lon), LatLng(lat + 0.1, lon + 0.25)], hPa: hPa);

    test('labels every 8 hPa far out, every 4 hPa close in', () {
      // Anchor on the lattice so only the hPa spacing decides.
      expect(isobarLabel(lineAt(0, 0, 1012), 0.6), isNull); // 1012 % 8 != 0
      expect(isobarLabel(lineAt(0, 0, 1016), 0.6), '1016');
      expect(isobarLabel(lineAt(0, 0, 1012), 0.4), '1012');
      // Half-step levels never label, at any zoom.
      expect(isobarLabel(lineAt(0, 0, 1010), 0.1), isNull);
    });

    test('only anchors near the lon/lat lattice get labels', () {
      // Close zoom: 1° lattice, 0.15° tolerance.
      expect(isobarLabel(lineAt(-70.05, 41.1, 1012), 0.4), '1012');
      expect(isobarLabel(lineAt(-70.5, 41.1, 1012), 0.4), isNull);
      expect(isobarLabel(lineAt(-70.05, 41.5, 1012), 0.4), isNull);
      // Far zoom: 5° lattice, 0.75° tolerance.
      expect(isobarLabel(lineAt(-70.5, 40.3, 1016), 0.6), '1016');
      expect(isobarLabel(lineAt(-72.5, 40.3, 1016), 0.6), isNull);
    });

    test('labelling is sparse over a realistic segment population', () {
      // 200 stubs along one isobar at lat 40, anchors every 0.25° like the
      // GFS grid: only the integer-longitude anchors hit the 1° lattice, so
      // labels land every 1° (every 4th stub) instead of one per 25 km stub.
      // (On the web, OL's declutter thins these further at render time.)
      final labelled = [
        for (var i = 0; i < 200; i++)
          if (isobarLabel(lineAt(-90 + i * 0.25, 40.0, 1012), 0.4) != null) i
      ];
      expect(labelled, hasLength(50));
      for (var j = 1; j < labelled.length; j++) {
        expect(labelled[j] - labelled[j - 1], 4);
      }
      // Off-lattice latitude: the whole contour stays unlabelled — the
      // latitude gate is what keeps labels off every east-west contour.
      expect(isobarLabel(lineAt(-70.0, 40.4, 1012), 0.4), isNull);
    });
  });
}
