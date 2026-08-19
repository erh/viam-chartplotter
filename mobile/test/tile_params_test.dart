import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/tile_sources.dart';

// A1: chart tile params must match the web app's tileUrlFunction
// (src/marineMap.svelte) — overview / mid / detail variants picked by tile
// zoom against VECTOR_TILE_NAVAID_MIN_Z=12 / VECTOR_TILE_STRUCTURE_MIN_Z=14.
void main() {
  group('chartTileParams with the web thresholds (12/14)', () {
    String at(int z) => chartTileParams(z, navaidMinZ: 12, structureMinZ: 14);

    test('overview below the navaid tier', () {
      for (final z in [7, 8, 9, 10, 11]) {
        expect(at(z), 'style=ecdis', reason: 'z$z');
      }
    });

    test('mid tier hands navaids to the vector layer', () {
      for (final z in [12, 13]) {
        expect(at(z), 'style=wms&navaids=0', reason: 'z$z');
      }
    });

    test('detail tier also skips structures', () {
      for (final z in [14, 15, 16]) {
        expect(at(z), 'style=wms&navaids=0&skip=BRIDGE,CBLOHD,PIPOHD,CONVYR',
            reason: 'z$z');
      }
    });
  });

  test('shipped defaults: navaid tier on at 12 (B1), structures still off',
      () {
    // B1 landed the navaids vector layer, so the tile stops baking navaids
    // at z >= 12; structures still render in the tile until B2.
    for (var z = 7; z < 12; z++) {
      expect(chartTileParams(z), 'style=ecdis', reason: 'z\$z');
    }
    for (var z = 12; z <= 16; z++) {
      expect(chartTileParams(z), 'style=wms&navaids=0', reason: 'z\$z');
    }
  });

  group('checkmate tile source', () {
    final checkmate = baseLayersFor('https://tiles.example').singleWhere((s) => s.id == 'checkmate');

    test('uses the per-zoom params builder and keeps the z>=7 gate', () {
      expect(checkmate.paramsForZoom, isNotNull);
      expect(checkmate.minZoom, 7);
    });

    test('full tile URLs carry the tier params and never landfill=', () {
      final provider = ZoomParamsTileProvider(checkmate.paramsForZoom!);
      final layer = TileLayer(urlTemplate: checkmate.urlTemplate);
      for (var z = 7; z <= 16; z++) {
        final url = provider.getTileUrl(TileCoordinates(1, 2, z), layer);
        expect(url, contains('/noaa-enc/tile/$z/1/2.png?style='), reason: 'z$z');
        expect(url, isNot(contains('landfill=')), reason: 'z$z');
      }
    });

    test('no base layer template mentions landfill', () {
      for (final s in baseLayersFor('https://tiles.example')) {
        expect(s.urlTemplate, isNot(contains('landfill=')), reason: s.id);
      }
    });
  });
}
