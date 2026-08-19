import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/chart/structures.dart';

void main() {
  test('bridge sheet shows category and clearances, matching web', () {
    final lines = structureSheetLines({
      'class': 'BRIDGE',
      'OBJNAM': 'Gold Star Memorial',
      'CATBRG': 5,
      'VERCLR': 41.1,
      'HORCLR': 100,
    });
    expect(lines[0], 'Gold Star Memorial');
    expect(lines[1], 'Bridge');
    expect(lines[2], 'Bascule · Vert clr 41.1 m · Horz clr 100.0 m');
  });

  test('opening bridge shows closed/open clearances', () {
    final lines = structureSheetLines({
      'class': 'BRIDGE',
      'CATBRG': 2,
      'VERCCL': 8.2,
      'VERCOP': 41.0,
    });
    expect(lines[2], 'Opening · Closed 8.2 m · Open 41.0 m');
  });

  test('overhead cable labels and INFORM fallback to NINFOM', () {
    expect(structureSheetLines({'class': 'CBLOHD', 'NINFOM': 'note'}),
        ['Overhead cable', 'Overhead cable', 'note']);
  });

  test('GeoJSON parse: point, line, polygon anchors', () {
    const body = '''
    {"features":[
      {"geometry":{"type":"Point","coordinates":[-72.1,41.2]},
       "properties":{"class":"CBLOHD"}},
      {"geometry":{"type":"LineString",
        "coordinates":[[-72.0,41.0],[-72.0,41.1]]},
       "properties":{"class":"BRIDGE"}},
      {"geometry":{"type":"Polygon",
        "coordinates":[[[-72.0,41.0],[-72.0,41.2],[-71.8,41.1],[-72.0,41.0]]]},
       "properties":{"class":"BRIDGE"}}
    ]}''';
    final f = parseStructureGeoJson(body);
    expect(f.length, 3);
    expect(f[0].traces, isEmpty);
    expect(f[1].anchor.latitude, 41.0); // line: first vertex
    expect(f[1].traces.single.length, 2);
    expect(f[2].isPolygon, isTrue);
    expect(f[2].anchor.latitude, closeTo(41.075, 1e-9)); // ring centroid
    expect(parseStructureGeoJson('junk'), isEmpty);
  });

  test('hideIcon suppresses the badge but keeps the trace', () {
    const body = '''
    {"features":[
      {"geometry":{"type":"LineString",
        "coordinates":[[-72.0,41.0],[-72.0,41.1]]},
       "properties":{"class":"BRIDGE","hideIcon":true}}
    ]}''';
    final f = parseStructureGeoJson(body).single;
    expect(f.hideIcon, isTrue);
    expect(f.traces, isNotEmpty);
  });
}
