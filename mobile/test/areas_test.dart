import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/chart/areas.dart';

void main() {
  test('areaVisibleToday: plain, open-ended, and year-wrapping windows', () {
    // Plain window.
    expect(areaVisibleToday('04-01', '06-30', today: '05-15'), isTrue);
    expect(areaVisibleToday('04-01', '06-30', today: '07-01'), isFalse);
    // Wrapping window (e.g. right-whale winter closure 11-01..04-30).
    expect(areaVisibleToday('11-01', '04-30', today: '12-25'), isTrue);
    expect(areaVisibleToday('11-01', '04-30', today: '02-14'), isTrue);
    expect(areaVisibleToday('11-01', '04-30', today: '08-19'), isFalse);
    // Open-ended.
    expect(areaVisibleToday('04-01', null, today: '03-01'), isFalse);
    expect(areaVisibleToday(null, '06-30', today: '07-15'), isFalse);
    expect(areaVisibleToday(null, null, today: '08-19'), isTrue);
    expect(areaVisibleToday('', '', today: '08-19'), isTrue);
  });

  test('areaDateSuffix mirrors the web wording', () {
    expect(areaDateSuffix('11-01', '04-30'), ' (11-01 → 04-30)');
    expect(areaDateSuffix('11-01', null), ' (from 11-01)');
    expect(areaDateSuffix(null, '04-30'), ' (until 04-30)');
    expect(areaDateSuffix(null, null), '');
  });

  test('parseCssColor: hex forms and fallback', () {
    expect(parseCssColor('#ff3b30'), const Color(0xFFFF3B30));
    expect(parseCssColor('#f00'), const Color(0xFFFF0000));
    expect(parseCssColor('purple-ish'), areaDefaultColor);
    expect(parseCssColor(null, fallback: const Color(0xFF123456)),
        const Color(0xFF123456));
  });

  test('parses FeatureCollection with per-feature color overrides', () {
    const gj = {
      'type': 'FeatureCollection',
      'features': [
        {
          'type': 'Feature',
          'properties': {'color': '#1446cc'},
          'geometry': {
            'type': 'Polygon',
            'coordinates': [
              [
                [-72.0, 41.0],
                [-72.0, 41.5],
                [-71.5, 41.5],
                [-72.0, 41.0],
              ]
            ]
          },
        },
        {
          'type': 'Feature',
          'properties': {},
          'geometry': {
            'type': 'LineString',
            'coordinates': [
              [-72.0, 41.0],
              [-71.0, 41.0],
            ]
          },
        },
        {
          'type': 'Feature',
          'geometry': {
            'type': 'Point',
            'coordinates': [-72.2, 41.2]
          },
        },
      ],
    };
    final geoms = parseAreaGeoJson(gj, areaDefaultColor);
    expect(geoms.length, 3);
    expect(geoms[0].isPoint || geoms[0].isLine, isFalse); // polygon
    expect(geoms[0].color, const Color(0xFF1446CC)); // override
    expect(geoms[1].isLine, isTrue);
    expect(geoms[1].color, areaDefaultColor);
    expect(geoms[2].isPoint, isTrue);
  });

  test('accepts a bare Geometry and garbage yields nothing', () {
    final bare = parseAreaGeoJson({
      'type': 'Polygon',
      'coordinates': [
        [
          [-72.0, 41.0],
          [-72.0, 41.5],
          [-71.5, 41.5],
        ]
      ]
    }, areaDefaultColor);
    expect(bare.length, 1);
    expect(parseAreaGeoJson('not json', areaDefaultColor), isEmpty);
    expect(parseAreaGeoJson(42, areaDefaultColor), isEmpty);
  });

  test('componentFolders reads ui_folder markers', () {
    const cfg = '''
    {"components": [
      {"name": "rw-delaware-bay", "ui_folder": {"name": "right-whale"}},
      {"name": "fuel-fwd"},
      {"name": "weird", "ui_folder": {}}
    ]}''';
    expect(componentFolders(cfg), {'rw-delaware-bay': 'right-whale'});
    expect(componentFolders('nope'), isEmpty);
  });
}
