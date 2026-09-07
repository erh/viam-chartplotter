import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;
import 'package:latlong2/latlong.dart';

/// Chart search client, ported from src/lib/chartSearch.ts (tests translated
/// alongside). Finds anything named in the NOAA ENC store — lights, wrecks,
/// canyons, channels, anchorages, harbours — and gives back somewhere to look
/// on the map. Search runs on the chart server (it owns the feature store);
/// this module shapes the request, debounces the type-ahead, and works out
/// how the map should frame a hit.

class SearchHit {
  const SearchHit({
    required this.name,
    required this.class_,
    required this.label,
    required this.cell,
    required this.lat,
    required this.lng,
    required this.bbox,
    required this.distanceMeters,
    this.area = '',
  });

  final String name;

  /// S-57 object class acronym, e.g. "LIGHTS".
  final String class_;

  /// That class in words ("Light"), or the acronym when we have no wording.
  final String label;
  final String cell;
  final double lat;
  final double lng;

  /// [minLon, minLat, maxLon, maxLat] — the feature's full extent.
  final List<double> bbox;

  /// Metres from the origin passed to the search, or -1 when none was.
  final double distanceMeters;

  /// Where the hit is for a human — "Newport, RI" — or empty when the
  /// server could not place it.
  final String area;

  LatLng get pos => LatLng(lat, lng);

  static SearchHit? fromJson(dynamic j) {
    if (j is! Map) return null;
    final lat = j['lat'];
    final lng = j['lng'];
    if (lat is! num || lng is! num) return null;
    final rawBbox = j['bbox'];
    final bbox = [
      if (rawBbox is List)
        for (final v in rawBbox)
          if (v is num) v.toDouble()
    ];
    return SearchHit(
      name: (j['name'] ?? '').toString(),
      class_: (j['class'] ?? '').toString(),
      label: (j['label'] ?? j['class'] ?? '').toString(),
      cell: (j['cell'] ?? '').toString(),
      lat: lat.toDouble(),
      lng: lng.toDouble(),
      bbox: bbox.length == 4
          ? bbox
          : [lng.toDouble(), lat.toDouble(), lng.toDouble(), lat.toDouble()],
      distanceMeters: (j['distance_meters'] as num?)?.toDouble() ?? -1,
      area: (j['area'] ?? '').toString(),
    );
  }
}

/// Builds the /noaa-enc/search query. Exposed for testing.
String searchUrl(
  String base,
  String q, {
  LatLng? origin,
  int? limit,
  String? objectClass,
}) {
  final p = <String, String>{'q': q};
  if (origin != null) {
    // Nearest-first only makes sense with somewhere to measure from. Without
    // it the server falls back to alphabetical.
    p['lat'] = '${origin.latitude}';
    p['lon'] = '${origin.longitude}';
  }
  if (limit != null && limit > 0) p['limit'] = '$limit';
  if (objectClass != null && objectClass.isNotEmpty) p['class'] = objectClass;
  return '$base/noaa-enc/search?${Uri(queryParameters: p).query}';
}

class SearchResponse {
  const SearchResponse({required this.hits, required this.matchedQuery});
  final List<SearchHit> hits;

  /// The terms that actually matched. When it differs from what was typed,
  /// the full phrase found nothing and this is a narrower answer — say so
  /// rather than present it as an exact match.
  final String matchedQuery;
}

Future<SearchResponse> searchChart(
  String base,
  String q, {
  LatLng? origin,
  int? limit,
  String? objectClass,
  http.Client? client,
}) async {
  final trimmed = q.trim();
  if (trimmed.isEmpty) return const SearchResponse(hits: [], matchedQuery: '');
  final url = Uri.parse(
      searchUrl(base, trimmed, origin: origin, limit: limit, objectClass: objectClass));
  final resp = await (client?.get(url) ?? http.get(url));
  if (resp.statusCode != 200) {
    var msg = 'search failed (${resp.statusCode})';
    try {
      final body = jsonDecode(resp.body);
      if (body is Map && body['error'] is String) msg = body['error'] as String;
    } catch (_) {
      // non-JSON error body; keep the status message
    }
    throw Exception(msg);
  }
  final body = jsonDecode(resp.body);
  final results = body is Map ? body['results'] : null;
  return SearchResponse(
    hits: [
      if (results is List)
        for (final r in results)
          if (SearchHit.fromJson(r) case final hit?) hit
    ],
    matchedQuery: body is Map && body['matched_query'] is String
        ? body['matched_query'] as String
        : trimmed,
  );
}

/// Shortest query worth sending. One or two letters match half the chart.
const int minQueryLength = 3;

/// How the map should frame a hit: a point feature (a buoy, a light) has no
/// extent, so centre on it at a close zoom; an area (a canyon, a channel)
/// does, so fit the whole thing. Under ~50 m across in either axis there is
/// nothing to fit to — fitting a degenerate extent zooms to maximum and
/// shows a blank tile.
({bool fit, double zoom}) framingFor(SearchHit hit) {
  const degenerateDeg = 0.0005;
  final spanLon = (hit.bbox[2] - hit.bbox[0]).abs();
  final spanLat = (hit.bbox[3] - hit.bbox[1]).abs();
  if (spanLon < degenerateDeg && spanLat < degenerateDeg) {
    return (fit: false, zoom: 15);
  }
  return (fit: true, zoom: 0);
}

/// Formats a hit's distance for the result row; empty when unknown.
String formatSearchDistance(double meters) {
  if (!(meters >= 0)) return '';
  final nm = meters / 1852;
  return nm < 10 ? '${nm.toStringAsFixed(1)} nm' : '${nm.round()} nm';
}

/// Debounces an async search so typing doesn't fire a request per keystroke,
/// and so a slow earlier response can never overwrite a newer one.
class SearchRunner<T> {
  SearchRunner(this._run, this._empty,
      {this.delay = const Duration(milliseconds: 250)});

  final Future<T> Function(String q) _run;

  /// What to report for a query too short to send.
  final T _empty;
  final Duration delay;

  Timer? _timer;
  int _seq = 0;

  void search(
    String q,
    void Function(T result, String q) onResult,
    void Function(Object e) onError,
  ) {
    _timer?.cancel();
    final mine = ++_seq; // stamp: a stale response is dropped, not rendered
    if (q.trim().length < minQueryLength) {
      onResult(_empty, q);
      return;
    }
    _timer = Timer(delay, () {
      _run(q).then(
        (result) {
          if (mine == _seq) onResult(result, q);
        },
        onError: (Object e) {
          if (mine == _seq) onError(e);
        },
      );
    });
  }

  void cancel() {
    _timer?.cancel();
    _seq++; // invalidate anything in flight
  }
}
