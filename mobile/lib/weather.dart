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

  /// Bilinearly-interpolated (u, v) for a lon/lat — smooth between grid cells
  /// so arrows can be drawn finer than the 0.25° data. Falls back to nearest at
  /// the grid edges. Null if fully out of range.
  ({double u, double v})? sampleInterp(double lon, double lat) {
    if (dx <= 0 || dy <= 0) return null;
    final l = ((lon - lo1) % 360 + 360) % 360;
    final fx = l / dx;
    final fy = (la1 - lat) / dy;
    final ix = fx.floor(), iy = fy.floor();
    final tx = fx - ix, ty = fy - iy;
    double? g(int gx, int gy, List<double> a) {
      if (gy < 0 || gy >= ny) return null;
      final wx = ((gx % nx) + nx) % nx;
      final idx = gy * nx + wx;
      if (idx < 0 || idx >= a.length) return null;
      return a[idx];
    }

    final u00 = g(ix, iy, u), u10 = g(ix + 1, iy, u);
    final u01 = g(ix, iy + 1, u), u11 = g(ix + 1, iy + 1, u);
    final v00 = g(ix, iy, v), v10 = g(ix + 1, iy, v);
    final v01 = g(ix, iy + 1, v), v11 = g(ix + 1, iy + 1, v);
    if (u00 == null ||
        u10 == null ||
        u01 == null ||
        u11 == null ||
        v00 == null ||
        v10 == null ||
        v01 == null ||
        v11 == null) {
      return sample(lon, lat); // fall back to nearest at edges
    }
    double bil(double a, double b, double c, double d) =>
        a * (1 - tx) * (1 - ty) +
        b * tx * (1 - ty) +
        c * (1 - tx) * ty +
        d * tx * ty;
    return (u: bil(u00, u10, u01, u11), v: bil(v00, v10, v01, v11));
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

/// One entry of the server's model catalogue (F2):
/// GET /noaa-weather/models → [{name, displayName, kind, domain, minFh,
/// maxFh, stepFh, disabled, reason}, …]. `kind` is "wind" | "wave" |
/// "isobars" | "lightning"; the slider's range/step come from here instead
/// of hardcoded GFS numbers.
class WeatherModel {
  const WeatherModel({
    required this.name,
    required this.displayName,
    required this.kind,
    this.domain = '',
    required this.minFh,
    required this.maxFh,
    required this.stepFh,
    this.disabled = false,
    this.reason,
  });

  final String name;
  final String displayName;
  final String kind;
  final String domain;
  final int minFh;
  final int maxFh;
  final int stepFh;
  final bool disabled;
  final String? reason;

  /// What the app assumes when /noaa-weather/models is unreachable — the
  /// legacy GFS product every server has.
  static const WeatherModel gfsFallback = WeatherModel(
      name: 'gfs',
      displayName: 'GFS',
      kind: 'wind',
      minFh: 0,
      maxFh: 240,
      stepFh: 3);

  /// Clamp a forecast hour into this model's range, snapped to its step.
  int clampFh(int fh) {
    if (stepFh <= 0) return fh.clamp(minFh, maxFh);
    final snapped = minFh + ((fh - minFh) / stepFh).round() * stepFh;
    return snapped.clamp(minFh, maxFh);
  }

  static WeatherModel? fromJson(dynamic j) {
    if (j is! Map) return null;
    final name = j['name'];
    if (name is! String || name.isEmpty) return null;
    int i(dynamic v, int fallback) => v is num ? v.toInt() : fallback;
    return WeatherModel(
      name: name,
      displayName: (j['displayName'] ?? name).toString(),
      kind: (j['kind'] ?? 'wind').toString(),
      domain: (j['domain'] ?? '').toString(),
      minFh: i(j['minFh'], 0),
      maxFh: i(j['maxFh'], 240),
      stepFh: i(j['stepFh'], 3),
      disabled: j['disabled'] == true,
      reason: j['reason']?.toString(),
    );
  }
}

/// Parse the catalogue body; garbage entries are dropped, an unusable body
/// throws (the fetch wrapper turns that into the GFS fallback).
List<WeatherModel> parseWeatherModels(String body) {
  final decoded = jsonDecode(body);
  if (decoded is! List) {
    throw const FormatException('models JSON is not an array');
  }
  return [
    for (final j in decoded)
      if (WeatherModel.fromJson(j) case final m?) m
  ];
}

/// The server's model catalogue, or `[gfsFallback]` when the endpoint is
/// unreachable/broken — the app must keep working against older servers.
Future<List<WeatherModel>> fetchWeatherModels(String tileBase,
    {http.Client? client}) async {
  try {
    final uri = Uri.parse('$tileBase/noaa-weather/models');
    final resp = await (client ?? http.Client())
        .get(uri)
        .timeout(const Duration(seconds: 10));
    if (resp.statusCode != 200) throw http.ClientException('${resp.statusCode}', uri);
    final models = parseWeatherModels(resp.body);
    return models.isEmpty ? const [WeatherModel.gfsFallback] : models;
  } catch (_) {
    return const [WeatherModel.gfsFallback];
  }
}

/// Fetch + decode the wind field for [model] (e.g. "gfs") at forecast hour
/// [fh] (0 = latest analysis). The server snaps fh to the model's step.
Future<WindField> fetchWindField(String tileBase, String model,
    {int fh = 0}) async {
  final uri =
      Uri.parse('$tileBase/noaa-weather/data/$model/latest.json?fh=$fh');
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
