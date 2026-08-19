import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/tile_sources.dart';

/// A2 (safe depth `sd`) and A3 (cache-buster `v`) on top of A1's tier params.
void main() {
  test('every chart URL carries v= from the build stamp', () {
    for (var z = 7; z <= 16; z++) {
      final q = chartTileUrlParams(z, buildStamp: '1.2.3+45');
      expect(q, contains('v=1.2.3%2B45'), reason: 'z$z');
      expect(q, startsWith('style='), reason: 'z$z');
    }
  });

  test('safe depth appends sd= when set', () {
    final q = chartTileUrlParams(9, buildStamp: 'dev', safeDepthFt: 6);
    expect(q, contains('&sd=6'));
    expect(chartTileUrlParams(9, buildStamp: 'dev', safeDepthFt: 20),
        contains('&sd=20'));
  });

  test('no safe depth means no sd param at all (never a bare sd=)', () {
    final q = chartTileUrlParams(9, buildStamp: 'dev');
    expect(q, isNot(contains('sd=')));
  });

  test('different build stamps produce different URLs', () {
    final a = chartTileUrlParams(9, buildStamp: '1.0.0+1');
    final b = chartTileUrlParams(9, buildStamp: '1.0.0+2');
    expect(a, isNot(equals(b)));
  });

  test('tier params still respected under the extras', () {
    final q = chartTileUrlParams(14,
        buildStamp: 'dev', navaidMinZ: 12, structureMinZ: 14);
    expect(q, startsWith('style=wms&navaids=0&skip='));
  });
}
