import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/boat_state.dart';

void main() {
  test('under way (SOG > 1 kn) COG wins over compass heading', () {
    final s = BoatState();
    s.update(speedKn: 6, cogDeg: 90, headingDeg: 75);
    expect(s.effectiveHeadingDeg, 90);
  });

  test('below 1 kn the compass heading wins (COG is GPS noise)', () {
    final s = BoatState();
    s.update(speedKn: 0.4, cogDeg: 312, headingDeg: 75);
    expect(s.effectiveHeadingDeg, 75);
  });

  test('under way with no COG falls back to heading', () {
    final s = BoatState();
    s.update(speedKn: 6, headingDeg: 75);
    expect(s.effectiveHeadingDeg, 75);
  });

  test('no speed reading counts as not moving', () {
    final s = BoatState();
    s.update(cogDeg: 90, headingDeg: 75);
    expect(s.effectiveHeadingDeg, 75);
  });
}
