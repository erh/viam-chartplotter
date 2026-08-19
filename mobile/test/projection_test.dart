import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/map/heading_line.dart';

void main() {
  const d = Distance();

  test('a 10 kn target 6-minute vector measures 1.0 nm', () {
    const from = LatLng(41.3, -72.0);
    final p = projectionPoints(from, 90, 10, 6)!;
    expect(d.distance(from, p.last) / 1852.0, closeTo(1.0, 0.005));
  });

  test('no COG or not moving draws no vector', () {
    const from = LatLng(41.3, -72.0);
    expect(projectionPoints(from, null, 10, 2), isNull);
    expect(projectionPoints(from, 90, 0, 2), isNull);
  });

  test('vector follows the COG', () {
    const from = LatLng(41.3, -72.0);
    final p = projectionPoints(from, 200, 8, 5)!;
    expect((d.bearing(from, p[1]) + 360) % 360, closeTo(200, 0.5));
  });
}
