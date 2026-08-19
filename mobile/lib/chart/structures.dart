import 'dart:convert';

import 'package:http/http.dart' as http;
import 'package:latlong2/latlong.dart';

/// Structures data (B2): bridges, overhead cables/pipes, conveyors from
/// /noaa-enc/structures. Ported from src/marineMap.svelte:1960-2127. The
/// tooltip content is the point — vertical clearance is what you need
/// before passing under something.

String structureClassLabel(String c) {
  switch (c) {
    case 'BRIDGE':
      return 'Bridge';
    case 'CBLOHD':
      return 'Overhead cable';
    case 'PIPOHD':
      return 'Overhead pipe';
    case 'CONVYR':
      return 'Conveyor';
    default:
      return c;
  }
}

/// S-57 CATBRG enum → human-readable bridge category.
String bridgeCategoryLabel(int code) {
  switch (code) {
    case 1:
      return 'Fixed';
    case 2:
      return 'Opening';
    case 3:
      return 'Swing';
    case 4:
      return 'Lifting';
    case 5:
      return 'Bascule';
    case 6:
      return 'Pontoon';
    case 7:
      return 'Drawbridge';
    case 8:
      return 'Transporter';
    case 9:
      return 'Footbridge';
    case 10:
      return 'Viaduct';
    case 11:
      return 'Aqueduct';
    case 12:
      return 'Suspension';
    default:
      return '';
  }
}

/// Sheet lines mirroring the web's formatStructureTooltip: title, class,
/// then category + clearances joined with ' · ', then INFORM/NINFOM.
List<String> structureSheetLines(Map<String, dynamic> props) {
  final class_ = (props['class'] ?? '').toString();
  final lines = <String>[
    props['OBJNAM'] != null
        ? props['OBJNAM'].toString()
        : structureClassLabel(class_),
    structureClassLabel(class_),
  ];
  final meta = <String>[];
  if (class_ == 'BRIDGE' && props['CATBRG'] != null) {
    final label =
        bridgeCategoryLabel(int.tryParse(props['CATBRG'].toString()) ?? -1);
    if (label.isNotEmpty) meta.add(label);
  }
  String fmtClr(dynamic v) =>
      '${(double.tryParse(v.toString()) ?? 0).toStringAsFixed(1)} m';
  if (props['VERCLR'] != null) meta.add('Vert clr ${fmtClr(props['VERCLR'])}');
  if (props['VERCCL'] != null) meta.add('Closed ${fmtClr(props['VERCCL'])}');
  if (props['VERCOP'] != null) meta.add('Open ${fmtClr(props['VERCOP'])}');
  if (props['VERCSA'] != null) meta.add('Safe ${fmtClr(props['VERCSA'])}');
  if (props['HORCLR'] != null) meta.add('Horz clr ${fmtClr(props['HORCLR'])}');
  if (meta.isNotEmpty) lines.add(meta.join(' · '));
  final info = props['INFORM'] ?? props['NINFOM'];
  if (info != null) lines.add(info.toString());
  return lines;
}

/// One structure: an icon anchor plus zero or more line traces (a bridge's
/// span, a cable's run). Polygon rings arrive as closed traces.
class StructureFeature {
  const StructureFeature({
    required this.anchor,
    required this.props,
    this.traces = const [],
    this.isPolygon = false,
  });

  final LatLng anchor;
  final Map<String, dynamic> props;
  final List<List<LatLng>> traces;
  final bool isPolygon;

  String get class_ => (props['class'] ?? '').toString();
  bool get hideIcon => props['hideIcon'] == true;
  bool get isBridge => class_ == 'BRIDGE';
}

List<LatLng>? _ring(dynamic coords) {
  if (coords is! List) return null;
  final pts = <LatLng>[];
  for (final c in coords) {
    if (c is! List || c.length < 2) continue;
    final lon = (c[0] as num?)?.toDouble();
    final lat = (c[1] as num?)?.toDouble();
    if (lon == null || lat == null) continue;
    pts.add(LatLng(lat, lon));
  }
  return pts.length >= 2 ? pts : null;
}

LatLng _centroid(List<LatLng> pts) {
  var lat = 0.0, lng = 0.0;
  for (final p in pts) {
    lat += p.latitude;
    lng += p.longitude;
  }
  return LatLng(lat / pts.length, lng / pts.length);
}

/// Parse the endpoint's FeatureCollection: Points, LineStrings, Polygons
/// and their Multi- variants. Icon anchor: the point itself, a line's
/// first vertex (web: predictable hover target), a polygon's centroid.
List<StructureFeature> parseStructureGeoJson(String body) {
  try {
    final decoded = jsonDecode(body);
    final features = (decoded as Map)['features'];
    if (features is! List) return const [];
    final out = <StructureFeature>[];
    for (final f in features) {
      if (f is! Map) continue;
      final geom = f['geometry'];
      if (geom is! Map) continue;
      final rawProps = f['properties'];
      final props = rawProps is Map
          ? rawProps.map((k, v) => MapEntry(k.toString(), v))
          : <String, dynamic>{};
      final type = geom['type'];
      final coords = geom['coordinates'];
      switch (type) {
        case 'Point':
          final p = _ring([coords]);
          if (p != null || coords is List && coords.length >= 2) {
            final lon = (coords[0] as num?)?.toDouble();
            final lat = (coords[1] as num?)?.toDouble();
            if (lon != null && lat != null) {
              out.add(StructureFeature(anchor: LatLng(lat, lon), props: props));
            }
          }
        case 'LineString':
          final line = _ring(coords);
          if (line != null) {
            out.add(StructureFeature(
                anchor: line.first, props: props, traces: [line]));
          }
        case 'MultiLineString':
          if (coords is List) {
            final lines = [
              for (final l in coords)
                if (_ring(l) case final r?) r
            ];
            if (lines.isNotEmpty) {
              out.add(StructureFeature(
                  anchor: lines.first.first, props: props, traces: lines));
            }
          }
        case 'Polygon':
          if (coords is List && coords.isNotEmpty) {
            final outer = _ring(coords.first);
            if (outer != null) {
              out.add(StructureFeature(
                  anchor: _centroid(outer),
                  props: props,
                  traces: [outer],
                  isPolygon: true));
            }
          }
        case 'MultiPolygon':
          if (coords is List) {
            for (final poly in coords) {
              if (poly is List && poly.isNotEmpty) {
                final outer = _ring(poly.first);
                if (outer != null) {
                  out.add(StructureFeature(
                      anchor: _centroid(outer),
                      props: props,
                      traces: [outer],
                      isPolygon: true));
                }
              }
            }
          }
      }
    }
    return out;
  } catch (_) {
    return const [];
  }
}

/// Fetch for a BboxFeatureSource (B4).
Future<List<StructureFeature>> fetchStructures(
  String tileBase,
  double west,
  double south,
  double east,
  double north,
) async {
  final uri = Uri.parse('$tileBase/noaa-enc/structures'
      '?minLon=$west&minLat=$south&maxLon=$east&maxLat=$north');
  final r = await http.get(uri).timeout(const Duration(seconds: 10));
  if (r.statusCode != 200) return const [];
  return parseStructureGeoJson(r.body);
}
