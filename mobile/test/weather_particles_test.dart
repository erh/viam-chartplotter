import 'package:flutter/painting.dart' show Color;
import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/map/weather_particles.dart';
import 'package:viam_chartplotter_mobile/weather.dart';

// Pins the particle configs to the web's ol-wind parameters
// (WeatherOverlays.svelte setupWindLayer / wave setup) so the two apps keep
// rendering the same weather picture.

void main() {
  test('wind config matches the web layer', () {
    const c = WeatherParticleConfig.wind;
    expect(c.paths, 2500);
    expect(c.particleAge, 100);
    expect(c.alpha, 0.82);
    expect(c.velocityFactor, 0.225);
    expect(c.maxValue, 15);
    // lineWidth: (m) => 2.7 + m * 0.11
    expect(c.lineWidthFor(0), closeTo(2.7, 1e-9));
    expect(c.lineWidthFor(15), closeTo(2.7 + 15 * 0.11, 1e-9));
    expect(c.lineWidthFor(-1), closeTo(2.7, 1e-9)); // clamped at calm
  });

  test('wave config matches the web layer', () {
    const c = WeatherParticleConfig.waves;
    expect(c.paths, 6000);
    expect(c.alpha, 0.97);
    expect(c.velocityFactor, 0.12);
    expect(c.maxValue, 3);
    expect(c.lineWidthFor(0), 7.5);
    expect(c.lineWidthFor(3), 7.5); // constant width for waves
  });

  test('wind colour scale matches the web stop-for-stop at the ends', () {
    expect(windColorScale, hasLength(15));
    expect(windColorScale.first, const Color(0xFF0a3d91)); // deep blue calm
    expect(windColorScale.last, const Color(0xFF7f0000)); // 28+ kt dark red
    expect(windRangeMaxMs, 15);
  });
}
