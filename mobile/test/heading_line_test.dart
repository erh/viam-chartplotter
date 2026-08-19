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
      // 0.5% tolerance: latlong2's offset (spherical) and distance
      // (ellipsoidal in some releases) use slightly different earth models,
      // and the resolved point-release differs between local and CI (the
      // app's pubspec.lock is gitignored).
      expect(meters, closeTo(nm * 1852.0, nm * 1852.0 * 0.005),
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

  test('ticks land every 1 nm, perpendicular to the line', () {
    const from = LatLng(41.3, -72.0);
    final ticks = headingLineTicks(from, 45, 5);
    expect(ticks.length, 5); // 1..5 nm inclusive
    for (var i = 0; i < ticks.length; i++) {
      final mid = LatLng(
        (ticks[i][0].latitude + ticks[i][1].latitude) / 2,
        (ticks[i][0].longitude + ticks[i][1].longitude) / 2,
      );
      expect(d.distance(from, mid), closeTo((i + 1) * 1852.0, (i + 1) * 12.0),
          reason: 'tick \${i + 1}');
      // Across the line: the tick's own bearing is ~heading ± 90.
      final tickBrg = (d.bearing(ticks[i][0], ticks[i][1]) + 360) % 360;
      expect((tickBrg - 315).abs() < 3 || (tickBrg - 135).abs() < 3, isTrue,
          reason: 'tick bearing \$tickBrg');
    }
  });
}
