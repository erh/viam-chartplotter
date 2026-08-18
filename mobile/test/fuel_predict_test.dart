import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/fuel_screen.dart';

// 1000 gal ≈ 3785 L: at that capacity 1%/min == 10 gal/min, keeping the
// %/min ↔ gal/min arithmetic legible in the cases below.
const capacity = 1000 * kLitersPerGallon;

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

  test('steady fill reports gal/min and time to 95%', () {
    // 1%/min of 1000 gal = 10 gal/min; from 60% it's 35 minutes to 95%.
    final r = tankRateReport(
      series: ramp(start: t0, from: 50, perMin: 1, minutes: 10),
      level: 60,
      capacityL: capacity,
    );
    expect(r, isNotNull);
    expect(r!.filling, isTrue);
    expect(r.gpm, closeTo(10, 0.5));
    expect(r.to95, isNotNull);
    expect(r.to95!.inMinutes, closeTo(35, 1));
  });

  test('rate comes from the last five minutes only', () {
    // Flat for 20 minutes, then 2%/min (20 gal/min) for 5.
    final flat = ramp(start: t0, from: 40, perMin: 0, minutes: 20);
    final fill = ramp(
        start: t0.add(const Duration(minutes: 20)),
        from: 40,
        perMin: 2,
        minutes: 5);
    final r = tankRateReport(
        series: [...flat, ...fill], level: 50, capacityL: capacity);
    expect(r, isNotNull);
    expect(r!.gpm, closeTo(20, 1));
  });

  test('draining over 1 gal/min reports usage', () {
    // -0.5%/min of 1000 gal = 5 gal/min burn.
    final r = tankRateReport(
      series: ramp(start: t0, from: 80, perMin: -0.5, minutes: 10),
      level: 75,
      capacityL: capacity,
    );
    expect(r, isNotNull);
    expect(r!.filling, isFalse);
    expect(r.gpm, closeTo(5, 0.3));
    expect(r.to95, isNull);
  });

  test('under 1 gal/min either way is ignored as noise', () {
    // ±0.05%/min of 1000 gal = ±0.5 gal/min — inside the deadband.
    for (final perMin in [0.05, -0.05, 0.0]) {
      expect(
        tankRateReport(
          series: ramp(start: t0, from: 50, perMin: perMin, minutes: 10),
          level: 50,
          capacityL: capacity,
        ),
        isNull,
        reason: 'rate $perMin%/min should be inside the deadband',
      );
    }
  });

  test('no report without enough history or a capacity', () {
    final good = ramp(start: t0, from: 50, perMin: 1, minutes: 10);
    // Under 2.5 minutes of history → no trend.
    expect(
        tankRateReport(
            series: ramp(start: t0, from: 50, perMin: 1, minutes: 2),
            level: 52,
            capacityL: capacity),
        isNull);
    expect(tankRateReport(series: const [], level: 50, capacityL: capacity),
        isNull);
    // No capacity → no gallon rate to judge against.
    expect(tankRateReport(series: good, level: 60, capacityL: null), isNull);
  });

  test('filling above 95% still reports the rate, without an ETA', () {
    final r = tankRateReport(
      series: ramp(start: t0, from: 90, perMin: 1, minutes: 10),
      level: 96,
      capacityL: capacity,
    );
    expect(r, isNotNull);
    expect(r!.filling, isTrue);
    expect(r.to95, isNull);
  });
}
