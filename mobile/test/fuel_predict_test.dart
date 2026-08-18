import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/fuel_screen.dart';

List<({DateTime t, double v})> ramp({
  required DateTime start,
  required double from,
  required double perMin,
  required int minutes,
  int stepSec = 5,
}) =>
    [
      for (var s = 0; s <= minutes * 60; s += stepSec)
        (t: start.add(Duration(seconds: s)), v: from + perMin * s / 60.0),
    ];

void main() {
  final t0 = DateTime(2026, 8, 18, 12);

  test('steady fill predicts time to 95%', () {
    // 50% rising 1%/min for 10 minutes → at 60%, 35 minutes to 95%.
    final p = predict95(ramp(start: t0, from: 50, perMin: 1, minutes: 10));
    expect(p, isNotNull);
    expect(p!.ratePerMin, closeTo(1.0, 0.05));
    expect(p.toTarget.inMinutes, closeTo(35, 1));
  });

  test('rate is taken from the last five minutes only', () {
    // Flat for 20 minutes, then filling at 2%/min for 5 → rate ≈ 2.
    final flat = ramp(start: t0, from: 40, perMin: 0, minutes: 20);
    final fill = ramp(
        start: t0.add(const Duration(minutes: 20)),
        from: 40,
        perMin: 2,
        minutes: 5);
    final p = predict95([...flat, ...fill]);
    expect(p, isNotNull);
    expect(p!.ratePerMin, closeTo(2.0, 0.1));
  });

  test('no prediction when flat, draining, near-full, or data-poor', () {
    expect(predict95(ramp(start: t0, from: 50, perMin: 0, minutes: 10)),
        isNull);
    expect(predict95(ramp(start: t0, from: 50, perMin: -1, minutes: 10)),
        isNull);
    expect(predict95(ramp(start: t0, from: 96, perMin: 1, minutes: 10)),
        isNull);
    // Under 2.5 minutes of history → not enough to extrapolate.
    expect(
        predict95(ramp(start: t0, from: 50, perMin: 1, minutes: 2)), isNull);
    expect(predict95(const []), isNull);
  });
}
