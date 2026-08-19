import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/chart/navaids.dart';

void main() {
  test('S-57 colour codes decode like the web table', () {
    expect(s57Colour('1'), const Color(0xFFFFFFFF));
    expect(s57Colour('3'), const Color(0xFFD9263A));
    expect(s57Colour('4'), const Color(0xFF1F9E49));
    expect(s57Colour(' 6 '), const Color(0xFFF5D011)); // trims
    expect(s57Colour('99'), navaidGrey); // unknown → grey
  });

  test('navaidColours: csv split, missing → grey', () {
    expect(navaidColours({'COLOUR': '3,1'}),
        [const Color(0xFFD9263A), const Color(0xFFFFFFFF)]);
    expect(navaidColours({}), [navaidGrey]);
  });

  test('class labels and light characteristics match the web', () {
    expect(navaidClassLabel('BOYLAT'), 'Lateral buoy');
    expect(navaidClassLabel('BCNSPP'), 'Special-purpose beacon');
    expect(navaidClassLabel('LIGHTS'), 'Light');
    expect(navaidClassLabel('XX'), 'XX'); // unknown passes through
    expect(lightCharLabel(2), 'Fl');
    expect(lightCharLabel(4), 'Q');
    expect(lightCharLabel(8), 'Iso');
    expect(lightCharLabel(12), 'FFl');
    expect(lightCharLabel(99), '');
  });

  test('colour letters join like the web (W/R/G/…)', () {
    expect(colourLetters('3,1'), 'RW');
    expect(colourLetters('4'), 'G');
    expect(colourLetters('42'), ''); // unknown filtered
  });

  test('a lit buoy sheet shows the light characteristic line', () {
    final lines = navaidSheetLines({
      'class': 'BOYLAT',
      'OBJNAM': 'G "1"',
      'lighted': true,
      'LIGHT_LITCHR': 2,
      'LIGHT_SIGPER': 4,
      'LIGHT_COLOUR': '4',
      'LIGHT_VALNMR': 3,
      'COLOUR': '4',
    });
    expect(lines[0], 'G "1"');
    expect(lines[1], 'Lateral buoy');
    expect(lines[2], 'Fl G 4s 3nm');
  });

  test('sector and INFORM lines appear when present', () {
    final lines = navaidSheetLines({
      'class': 'LIGHTS',
      'LITCHR': 9,
      'SIGPER': 6,
      'COLOUR': '1',
      'HEIGHT': 20, // metres → 66 ft
      'SECTR1': 90,
      'SECTR2': 270,
      'INFORM': 'Private aid',
    });
    expect(lines[2], 'Oc W 6s 66ft');
    expect(lines[3], 'Sector 90°–270°');
    expect(lines[4], 'Private aid');
  });

  test('GeoJSON parses points and skips junk', () {
    const body = '''
    {"type":"FeatureCollection","features":[
      {"type":"Feature","geometry":{"type":"Point","coordinates":[-72.1,41.2]},
       "properties":{"class":"BOYLAT","COLOUR":"4","BOYSHP":2}},
      {"type":"Feature","geometry":{"type":"LineString","coordinates":[]}},
      {"type":"Feature","geometry":{"type":"Point","coordinates":[181]}},
      "junk"
    ]}''';
    final f = parseNavaidGeoJson(body);
    expect(f.length, 1);
    expect(f.single.class_, 'BOYLAT');
    expect(f.single.shape, 2);
    expect(f.single.pos.latitude, 41.2);
    expect(parseNavaidGeoJson('not json'), isEmpty);
  });
}
