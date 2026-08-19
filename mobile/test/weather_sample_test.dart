import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/boat_state.dart';
import 'package:viam_chartplotter_mobile/map/wind_overlay.dart';
import 'package:viam_chartplotter_mobile/weather.dart';

// F7 — point weather sample: "from" direction math and the callout lines,
// matching the web's cursor readout.

WindField uniformField(double u, double v) => WindField(
      nx: 4,
      ny: 4,
      lo1: -72,
      la1: 42,
      dx: 0.25,
      dy: 0.25,
      u: List.filled(16, u),
      v: List.filled(16, v),
    );

void main() {
  test('wind blowing toward south reads as "from" north', () {
    final c = WindOverlayController(state: BoatState())
      ..on = true
      ..field = uniformField(0, -5); // v negative = moving south
    final s = c.samplePoint(-71.9, 41.9);
    expect(s.windFromDeg, closeTo(0, 0.5)); // from the north
    expect(s.windKt, closeTo(5 * 1.94384, 0.01));
    expect(s.waveM, isNull); // waves off
  });

  test('westerly (moving east) reads as from 270', () {
    final c = WindOverlayController(state: BoatState())
      ..on = true
      ..field = uniformField(5, 0);
    expect(c.samplePoint(-71.9, 41.9).windFromDeg, closeTo(270, 0.5));
  });

  test('fields only report when their overlay is on', () {
    final c = WindOverlayController(state: BoatState())
      ..field = uniformField(5, 0) // loaded but overlay off
      ..wavesOn = true
      ..waveField = uniformField(0.6, 0.8); // |h| = 1.0 m
    final s = c.samplePoint(-71.9, 41.9);
    expect(s.windKt, isNull);
    expect(s.waveM, closeTo(1.0, 1e-9));
  });

  group('weatherSampleLines', () {
    test('formats like the web readout, zero-padded degrees', () {
      expect(
        weatherSampleLines(
          rangeNm: 4.204,
          bearingDeg: 130,
          windKt: 12.4,
          windFromDeg: 45,
          waveM: 1.0,
          waveFromDeg: 180,
        ),
        [
          '4.20 nm @ 130°',
          'wind 12 kt from 045°',
          'wave 3.3 ft from 180°', // 1.0 m × 3.28084
        ],
      );
    });

    test('null inputs drop their lines', () {
      expect(weatherSampleLines(windKt: 8, windFromDeg: 90),
          ['wind 8 kt from 090°']);
      expect(weatherSampleLines(), isEmpty);
    });
  });
}
