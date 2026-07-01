import 'dart:async';
import 'dart:convert';
import 'dart:math' as math;

import 'package:http/http.dart' as http;

/// A GFS-shape wind field decoded from the server's
/// /noaa-weather/data/{model}/latest.json (the same ol-wind JSON the web app
/// consumes). Two records — U (parameterNumber 2) and V (3) — over a regular
/// lat/lon grid. Data is row-major from the NW corner (la1, lo1), scanning
/// east then south.
class WindField {
  WindField({
    required this.nx,
    required this.ny,
    required this.lo1,
    required this.la1,
    required this.dx,
    required this.dy,
    required this.u,
    required this.v,
  });

  final int nx, ny;
  final double lo1, la1, dx, dy;
  final List<double> u, v;

  /// Nearest-cell (u, v) in m/s for a lon/lat, or null if out of range. Handles
  /// the grid's 0–360 longitude convention.
  ({double u, double v})? sample(double lon, double lat) {
    if (dx <= 0 || dy <= 0) return null;
    final l = ((lon - lo1) % 360 + 360) % 360;
    final ix = (l / dx).round() % nx;
    final iy = ((la1 - lat) / dy).round();
    if (iy < 0 || iy >= ny || ix < 0 || ix >= nx) return null;
    final idx = iy * nx + ix;
    if (idx < 0 || idx >= u.length || idx >= v.length) return null;
    return (u: u[idx], v: v[idx]);
  }

  /// Wind speed in knots for a lon/lat, or null.
  double? speedKnots(double lon, double lat) {
    final s = sample(lon, lat);
    if (s == null) return null;
    return math.sqrt(s.u * s.u + s.v * s.v) * 1.94384;
  }

  static WindField _fromJson(List<dynamic> records) {
    Map<String, dynamic>? uRec, vRec;
    for (final r in records) {
      if (r is! Map) continue;
      final h = r['header'];
      if (h is! Map) continue;
      final pn = h['parameterNumber'];
      if (pn == 2) uRec = r.cast<String, dynamic>();
      if (pn == 3) vRec = r.cast<String, dynamic>();
    }
    if (uRec == null || vRec == null) {
      throw const FormatException('wind JSON missing U/V records');
    }
    final h = (uRec['header'] as Map).cast<String, dynamic>();
    double d(dynamic x) => (x as num).toDouble();
    List<double> arr(dynamic x) =>
        (x as List).map((e) => (e as num).toDouble()).toList();
    return WindField(
      nx: (h['nx'] as num).toInt(),
      ny: (h['ny'] as num).toInt(),
      lo1: d(h['lo1']),
      la1: d(h['la1']),
      dx: d(h['dx']),
      dy: d(h['dy']),
      u: arr(uRec['data']),
      v: arr(vRec['data']),
    );
  }
}

/// Fetch + decode the wind field for [model] (e.g. "gfs") from the tile server.
Future<WindField> fetchWindField(String tileBase, String model) async {
  final uri = Uri.parse('$tileBase/noaa-weather/data/$model/latest.json');
  // Without a timeout a non-responsive weather server leaves the toggle
  // spinning forever ("nothing happens"); surface it as an error instead.
  final resp = await http.get(uri).timeout(const Duration(seconds: 15));
  if (resp.statusCode != 200) {
    throw http.ClientException('weather ${resp.statusCode}', uri);
  }
  final decoded = jsonDecode(resp.body);
  if (decoded is! List) {
    throw const FormatException('weather JSON is not an array');
  }
  return WindField._fromJson(decoded);
}
