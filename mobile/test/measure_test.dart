import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/map/measure.dart';

void main() {
  test('a known 10 nm leg measures 10.0 ± 0.05 nm', () {
    // 10 nm = 10 arc-minutes of latitude, due north.
    const a = LatLng(41.0, -72.0);
    const b = LatLng(41.0 + 10.0 / 60.0, -72.0);
    expect(distanceNm(a, b), closeTo(10.0, 0.05));
  });

  test('bearing matches the web formula for the cardinal directions', () {
    const a = LatLng(41.0, -72.0);
    expect(bearingDeg(a, const LatLng(42.0, -72.0)), closeTo(0, 0.01));
    expect(bearingDeg(a, const LatLng(40.0, -72.0)), closeTo(180, 0.01));
    // East/west have a slight great-circle deviation at 41°N.
    expect(bearingDeg(a, const LatLng(41.0, -71.0)), closeTo(90, 0.5));
    expect(bearingDeg(a, const LatLng(41.0, -73.0)), closeTo(270, 0.5));
  });

  test('bearing is in [0, 360) and label formats', () {
    const a = LatLng(41.0, -72.0);
    const b = LatLng(41.1, -72.1);
    final brg = bearingDeg(a, b);
    expect(brg, inInclusiveRange(0, 360));
    expect(measureLabel(a, b), matches(r'^\d+\.\d\d nm @ \d{3}°T$'));
  });
}
