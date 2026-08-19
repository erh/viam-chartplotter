import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/viam_connection.dart';

// L3 — adaptive poll cadence. The 1 Hz timer always fires (stuck-poll
// detection stays second-granular); this schedule decides which fires run a
// sweep. Derived per-hour sweep counts double as the data-budget baseline:
//   under way:   3600 sweeps/h (720 AIS fetches at the %5 stagger)
//   at anchor:    720 sweeps/h (144 AIS fetches) — 5× less
//   low data:    under way 1800/h, at anchor 360/h
void main() {
  test('full 1 Hz under way', () {
    expect(ViamConnection.pollEvery(moving: true, lowData: false), 1);
  });

  test('backs off hard at anchor', () {
    expect(ViamConnection.pollEvery(moving: false, lowData: false), 5);
  });

  test('low data stretches both', () {
    expect(ViamConnection.pollEvery(moving: true, lowData: true), 2);
    expect(ViamConnection.pollEvery(moving: false, lowData: true), 10);
  });

  test('anchor cadence is a strict multiple of under-way (measurably less)',
      () {
    for (final lowData in [false, true]) {
      final moving = ViamConnection.pollEvery(moving: true, lowData: lowData);
      final anchored =
          ViamConnection.pollEvery(moving: false, lowData: lowData);
      expect(anchored, greaterThanOrEqualTo(5 * moving));
    }
  });
}
