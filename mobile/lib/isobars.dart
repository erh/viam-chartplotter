import 'dart:convert';

import 'package:http/http.dart' as http;
import 'package:latlong2/latlong.dart';

/// GFS PRMSL isobars (F4), ported from src/lib/isobarLayer.ts.
///
/// The contours are NOT computed client-side: the Go server
/// (weather/noaa_isobars.go) runs marching squares over the PRMSL grid and
/// serves a ready-made GeoJSON FeatureCollection from
/// /noaa-weather/data/gfs-isobars/latest.json?fh=<fh>. Each LineString
/// feature is one short (cell, level) segment — the server doesn't stitch
/// them into long polylines. At 0.25° GFS resolution the segments are
/// ~25 km and the eye reads them as continuous contours, so we render them
/// as-is like the web app does. Point features are the H/L pressure
/// extremum markers.
///
/// This file is data plumbing plus the web's pure zoom/label ladder; the
/// map wiring lives elsewhere.

/// One contour segment at a pressure level.
class IsobarLine {
  const IsobarLine({required this.points, required this.hPa});

  final List<LatLng> points;

  /// Contour level in hectopascals.
  final int hPa;

  /// Where a label for this segment would sit. The web labels at the
  /// feature's first coordinate (isobarStyle's getFirstCoordinate), so the
  /// lattice sampling in [isobarLabel] stays stable as the user pans.
  LatLng get labelAnchor => points.first;
}

/// An "H" or "L" pressure extremum marker.
class IsobarExtremum {
  const IsobarExtremum({
    required this.position,
    required this.hPa,
    required this.kind,
  });

  final LatLng position;

  /// Field value at the extremum, hectopascals.
  final int hPa;

  /// "H" (red on the web) or "L" (blue) — the surface-analysis convention.
  final String kind;
}

/// A decoded isobar payload: contour segments, extremum markers, and the
/// server's `meta` block (GFS run time, forecast hour, contour spacing).
class IsobarField {
  const IsobarField({
    required this.lines,
    required this.extrema,
    this.refTime,
    this.forecastTime,
    this.stepHPa = 2,
  });

  final List<IsobarLine> lines;
  final List<IsobarExtremum> extrema;

  /// Reference time (GFS run) as an ISO string, from meta.refTime.
  final String? refTime;

  /// Forecast hour of this frame, from meta.forecastTime.
  final int? forecastTime;

  /// Contour spacing the server used (meta.stepHPa; 2 on current servers).
  final int stepHPa;
}

/// Parse the server's GeoJSON body. Throws [FormatException] when the body
/// isn't a FeatureCollection; individual malformed features are skipped so
/// one bad record can't blank the whole layer.
IsobarField parseIsobars(String body) {
  final decoded = jsonDecode(body);
  if (decoded is! Map || decoded['type'] != 'FeatureCollection') {
    throw const FormatException('isobar JSON is not a FeatureCollection');
  }
  final rawFeatures = decoded['features'];
  if (rawFeatures is! List) {
    throw const FormatException('isobar JSON has no features array');
  }

  final lines = <IsobarLine>[];
  final extrema = <IsobarExtremum>[];
  for (final f in rawFeatures) {
    if (f is! Map) continue;
    final geom = f['geometry'];
    final props = f['properties'];
    if (geom is! Map || props is! Map) continue;
    final hPaRaw = props['hPa'];
    if (hPaRaw is! num) continue;
    final hPa = hPaRaw.toInt();

    // GeoJSON coordinates are [lon, lat]; LatLng wants (lat, lon).
    LatLng? point(dynamic c) {
      if (c is! List || c.length < 2) return null;
      final lon = c[0], lat = c[1];
      if (lon is! num || lat is! num) return null;
      return LatLng(lat.toDouble(), lon.toDouble());
    }

    switch (geom['type']) {
      case 'LineString':
        final coords = geom['coordinates'];
        if (coords is! List) continue;
        final pts = <LatLng>[];
        for (final c in coords) {
          final p = point(c);
          if (p != null) pts.add(p);
        }
        if (pts.length < 2) continue;
        lines.add(IsobarLine(points: pts, hPa: hPa));
      case 'Point':
        final p = point(geom['coordinates']);
        final kind = props['kind'];
        if (p == null || (kind != 'H' && kind != 'L')) continue;
        extrema.add(IsobarExtremum(position: p, hPa: hPa, kind: kind));
    }
  }

  final meta = decoded['meta'];
  String? refTime;
  int? forecastTime;
  var stepHPa = 2;
  if (meta is Map) {
    final rt = meta['refTime'];
    if (rt is String) refTime = rt;
    final ft = meta['forecastTime'];
    if (ft is num) forecastTime = ft.toInt();
    final step = meta['stepHPa'];
    if (step is num && step > 0) stepHPa = step.toInt();
  }

  return IsobarField(
    lines: lines,
    extrema: extrema,
    refTime: refTime,
    forecastTime: forecastTime,
    stepHPa: stepHPa,
  );
}

/// Fetch + decode the isobar frame for forecast hour [fh]. Same endpoint
/// pattern and timeout discipline as fetchWindField in weather.dart —
/// without a timeout a dead weather server leaves the toggle spinning
/// forever. Throws on HTTP failure or a malformed body.
Future<IsobarField> fetchIsobars(String tileBase,
    {String model = 'gfs-isobars', int fh = 0, http.Client? client}) async {
  final uri =
      Uri.parse('$tileBase/noaa-weather/data/$model/latest.json?fh=$fh');
  final resp = await (client ?? http.Client())
      .get(uri)
      .timeout(const Duration(seconds: 15));
  if (resp.statusCode != 200) {
    throw http.ClientException('isobars ${resp.statusCode}', uri);
  }
  return parseIsobars(resp.body);
}

// --- Zoom / label ladder, ported verbatim from isobarStyle ------------------
//
// "Resolution" throughout is degrees of longitude per screen pixel — the
// OpenLayers view resolution under useGeographic. The map wiring converts
// flutter_map's zoom to this before calling in, so the thresholds below
// stay byte-for-byte comparable with the web.

/// The in-between 2 hPa contours the backend emits on top of the 4 hPa
/// marine standard. They get a hairline stroke and never carry labels.
bool isHalfStepHPa(int hPa) => hPa % 4 != 0;

/// Contour-line visibility ladder: at overview zoom (resolution > 0.3°/px)
/// the 2 hPa in-between lines are dropped entirely — only the 4 hPa
/// marine-standard contours show; close in, every line draws.
bool isobarLineVisible(int hPa, double resolution) =>
    !(isHalfStepHPa(hPa) && resolution > 0.3);

/// Stroke tier for a contour level. The web's buildStyle ladder:
/// reference (1000 hPa) darkest/2.0 px, heavy (every 20 hPa) 1.4 px,
/// standard (every 4 hPa) 1.0 px, half-step (2 hPa fill) hairline 0.6 px.
/// Order matters: 1000 is also %20==0, reference wins.
enum IsobarTier { reference, heavy, standard, half }

IsobarTier isobarTier(int hPa) {
  if (hPa == 1000) return IsobarTier.reference;
  if (hPa % 20 == 0) return IsobarTier.heavy;
  if (isHalfStepHPa(hPa)) return IsobarTier.half;
  return IsobarTier.standard;
}

/// Label text for a segment at the given resolution, or null for the vast
/// majority that stay unlabelled. Labels are sparser than lines — every
/// 8 hPa far out, every 4 hPa close in — and only segments whose anchor
/// falls on a coarse lon/lat lattice (~5° far, 1° close, 15% tolerance)
/// get text, so neighbouring 25 km stubs along the same isobar don't all
/// say "1012". The lattice is fixed in world coordinates, so the chosen
/// segments don't move around as the user pans.
String? isobarLabel(IsobarLine line, double resolution) {
  if (isHalfStepHPa(line.hPa)) return null;
  final labelEvery = resolution > 0.5 ? 8 : 4;
  if (line.hPa % labelEvery != 0) return null;
  final step = resolution > 0.5 ? 5.0 : 1.0;
  final lon = line.labelAnchor.longitude;
  final lat = line.labelAnchor.latitude;
  final lonHit = (lon - (lon / step).roundToDouble() * step).abs() < step * 0.15;
  final latHit = (lat - (lat / step).roundToDouble() * step).abs() < step * 0.15;
  if (!lonHit || !latHit) return null;
  return '${line.hPa}';
}
