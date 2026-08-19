import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/moon.dart';

// The web test compares Date.toUTCString(), which truncates to whole seconds;
// this reproduces that comparison granularity.
DateTime _truncToSecond(DateTime d) {
  final u = d.toUtc();
  return DateTime.utc(u.year, u.month, u.day, u.hour, u.minute, u.second);
}

void main() {
  // Reference values from the upstream SunCalc test suite.
  test('returns the correct moon rise/set times', () {
    final times =
        getMoonTimes(DateTime.utc(2013, 3, 4), 50.5, 30.5, inUTC: true);
    expect(_truncToSecond(times.rise!), DateTime.utc(2013, 3, 4, 23, 54, 29));
    expect(_truncToSecond(times.set!), DateTime.utc(2013, 3, 4, 7, 47, 58));
    expect(times.alwaysUp, isFalse);
    expect(times.alwaysDown, isFalse);
  });

  test('handles days where the moon does not rise', () {
    // High-arctic winter: moon stays below the horizon around new moon.
    final times = getMoonTimes(DateTime.utc(2023, 1, 21), 80, 0, inUTC: true);
    expect(times.rise, isNull);
    expect(times.set, isNull);
    expect(times.alwaysDown, isTrue);
    expect(times.alwaysUp, isFalse);
  });

  test('handles days where the moon stays up', () {
    // High-arctic winter around full moon: moon circles above the horizon.
    final times = getMoonTimes(DateTime.utc(2023, 1, 6), 80, 0, inUTC: true);
    expect(times.rise, isNull);
    expect(times.set, isNull);
    expect(times.alwaysUp, isTrue);
    expect(times.alwaysDown, isFalse);
  });
}
