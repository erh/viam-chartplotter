import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/boat_state.dart';
import 'package:viam_chartplotter_mobile/fuel_screen.dart';

// What these pin down: a dropped connection must not leave the fuel rate line
// missing once the boat is back. Two ways it used to:
//
//  1. Levels carried forward through the outage were recorded into history
//     with current timestamps. The rate's five-minute window then held nothing
//     but a flat line, which computes as "burning nothing" — under the 1
//     gal/min noise floor, so no rate is reported at all.
//  2. A tank whose read failed dropped out of state.tanks, and with it the
//     capacity carried forward from an earlier reading (the sensor only sends
//     `Capacity` intermittently). No capacity, no gallons, no rate — for the
//     rest of the session.
void main() {
  group('tank history', () {
    test('a fresh level is recorded', () {
      final s = BoatState();
      s.setSystems(tanks: [
        const TankStatus(name: 'main', level: 50, capacity: 3785),
      ]);
      expect(s.series('tank:main'), hasLength(1));
      expect(s.series('tank:main').single.v, 50);
    });

    test('a carried-forward level is NOT recorded', () {
      final s = BoatState();
      s.setSystems(tanks: [
        const TankStatus(name: 'main', level: 50, capacity: 3785),
      ]);
      // The boat stopped answering: same level, carried forward.
      for (var i = 0; i < 5; i++) {
        s.setSystems(tanks: [
          const TankStatus(
            name: 'main',
            level: 50,
            capacity: 3785,
            levelIsFresh: false,
          ),
        ]);
      }
      expect(s.series('tank:main'), hasLength(1),
          reason: 'an outage must not manufacture history');
      // The level itself is still shown — only the history is left alone.
      expect(s.tanks.single.level, 50);
    });

    test('carry-forward keeps the capacity the rate needs', () {
      final s = BoatState();
      s.setSystems(tanks: [
        const TankStatus(name: 'main', level: 50, capacity: 3785),
      ]);
      s.setSystems(tanks: [
        const TankStatus(
          name: 'main',
          level: 50,
          capacity: 3785,
          levelIsFresh: false,
        ),
      ]);
      expect(s.tanks.single.capacity, 3785);
    });
  });

  group('fuel rate across a reconnect', () {
    // 1000 gal in liters: at that capacity 1%/min is 10 gal/min.
    const capacity = 1000 * kLitersPerGallon;
    final t0 = DateTime(2026, 8, 19, 12);

    List<({DateTime t, double v})> burn({
      required DateTime from,
      required double startLevel,
      required int minutes,
    }) =>
        [
          for (var s = 0; s <= minutes * 60; s += 5)
            (t: from.add(Duration(seconds: s)), v: startLevel - 0.2 * s / 60.0),
        ];

    test('a real burn is reported once enough post-reconnect history exists',
        () {
      // Ten minutes of burn, a ten-minute outage with nothing recorded, then
      // three minutes back — enough to re-establish the trend.
      final series = [
        ...burn(from: t0, startLevel: 80, minutes: 10),
        ...burn(
          from: t0.add(const Duration(minutes: 20)),
          startLevel: 74,
          minutes: 3,
        ),
      ];
      final report = tankRateReport(
        series: series,
        level: 73.4,
        capacityL: capacity,
      );
      expect(report, isNotNull);
      expect(report!.filling, isFalse);
      expect(report.gpm, closeTo(2, 0.2)); // 0.2%/min of 1000 gal
    });

    test('an outage recorded as flat samples would erase the rate', () {
      // The pre-fix behaviour, kept as a regression witness: fill the window
      // with carried-forward duplicates and the same burn reports nothing.
      final resumeAt = t0.add(const Duration(minutes: 20));
      final series = [
        ...burn(from: t0, startLevel: 80, minutes: 10),
        for (var s = 0; s <= 10 * 60; s += 5)
          (t: t0.add(Duration(minutes: 10, seconds: s)), v: 78.0),
        ...burn(from: resumeAt, startLevel: 78, minutes: 1),
      ];
      final report = tankRateReport(
        series: series,
        level: 77.8,
        capacityL: capacity,
      );
      expect(report, isNull,
          reason: 'flat carried-forward samples read as "burning nothing"');
    });
  });
}
