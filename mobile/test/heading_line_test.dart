import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/map/heading_line.dart';

void main() {
  const d = Distance();

  test('endpoint is the selected distance away along the heading', () {
    const from = LatLng(41.3, -72.0);
    for (final nm in headingLineLengthChoices) {
      final pts = headingLinePoints(from, 45, nm.toDouble());
      expect(pts.first, from);
      final meters = d.distance(from, pts.last);
      expect(meters, closeTo(nm * 1852.0, nm * 1852.0 * 0.001),
          reason: '$nm nm');
    }
  });

  test('initial bearing matches the heading', () {
    const from = LatLng(41.3, -72.0);
    final pts = headingLinePoints(from, 100, 15);
    final b = d.bearing(from, pts[1]);
    expect((b + 360) % 360, closeTo(100, 0.5));
  });

  test('a 15 nm line is sampled, not a flat 2-point segment', () {
    final pts = headingLinePoints(const LatLng(41.3, -72.0), 100, 15);
    expect(pts.length, greaterThan(4));
  });
}
