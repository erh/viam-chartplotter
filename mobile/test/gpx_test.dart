import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/gpx.dart';

// gpx.test.ts translated (E4).

void main() {
  group('routeToGpx', () {
    test('emits a route with named points and 1e-7 precision', () {
      final xml = routeToGpx('Block Island Run', const [
        LatLng(41.1631, -71.5784),
        LatLng(41.2042, -71.5511),
      ]);
      expect(xml, contains('<gpx version="1.1"'));
      expect(xml, contains('<name>Block Island Run</name>'));
      expect(xml, contains('<rtept lat="41.1631000" lon="-71.5784000">'));
      expect(xml, contains('<name>Block Island Run 01</name>'));
      expect(xml, contains('<name>Block Island Run 02</name>'));
      expect('<rtept '.allMatches(xml).length, 2);
    });

    test('escapes XML in names', () {
      final xml = routeToGpx("Tom & Jerry's <run>", const [LatLng(1, 2)]);
      expect(
          xml, contains('<name>Tom &amp; Jerry&apos;s &lt;run&gt;</name>'));
      expect(xml, isNot(contains('<run>')));
    });

    test('falls back to a default name', () {
      final xml = routeToGpx('  ', const [LatLng(1, 2)]);
      expect(xml, contains('<name>Route</name>'));
      expect(xml, contains('<name>Route 01</name>'));
    });
  });

  group('gpxFilename', () {
    test('slugifies', () {
      expect(gpxFilename('Block Island Run'), 'block-island-run.gpx');
      expect(gpxFilename('  A / B?  '), 'a-b.gpx');
      expect(gpxFilename('!!!'), 'route.gpx');
    });
  });
}
