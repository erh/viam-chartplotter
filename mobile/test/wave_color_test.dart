import 'package:flutter/painting.dart' show Color;
import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/weather.dart';

// F3 — wave colour scale + colorForValue, ported from src/lib/windLayer.ts.
// Both apps must colour the same sea state identically.

void main() {
  test('scale matches the web stop-for-stop at the ends', () {
    expect(waveColorScale, hasLength(15));
    expect(waveColorScale.first, const Color(0xFFf0f7ff)); // calm
    expect(waveColorScale.last, const Color(0xFF6e0606)); // 3 m+ deep red
    expect(waveRangeMaxM, 3.0);
  });

  test('endpoints clamp', () {
    expect(colorForValue(waveColorScale, 0, waveRangeMaxM),
        waveColorScale.first);
    expect(colorForValue(waveColorScale, 99, waveRangeMaxM),
        waveColorScale.last);
    expect(colorForValue(waveColorScale, -1, waveRangeMaxM),
        waveColorScale.first);
  });

  test('garbage input falls back to the first stop (web behaviour)', () {
    expect(colorForValue(waveColorScale, double.nan, waveRangeMaxM),
        waveColorScale.first);
    expect(colorForValue(waveColorScale, 1, 0), waveColorScale.first);
    expect(colorForValue(const [], 1, 3), const Color(0xFF000000));
  });

  test('interpolates linearly between adjacent stops', () {
    // Halfway between stop 0 and stop 1 of a two-colour scale.
    const scale = [Color(0xFF000000), Color(0xFF80402A)];
    final mid = colorForValue(scale, 0.5, 1.0);
    expect((mid.r * 255).round(), closeTo(0x40, 1));
    expect((mid.g * 255).round(), closeTo(0x20, 1));
    expect((mid.b * 255).round(), closeTo(0x15, 1));
  });

  test('legend ticks read in feet: 0/2/5/7/10 for the 3 m ramp', () {
    final ticks = [
      for (final f in [0.0, 0.25, 0.5, 0.75, 1.0])
        (waveRangeMaxM * f * metersToFeet).round()
    ];
    expect(ticks, [0, 2, 5, 7, 10]);
  });
}
