import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math' as math;

import 'package:http/http.dart' as http;

import 'boat_state.dart';

/// Tides (G1), ported from the web app (src/marineMap.svelte:5281-5550):
/// nearest NOAA station to the boat, current level interpolated from the
/// hourly predictions, next high/low, and a series for the sparkline. NOAA
/// is called directly, not through the Go module.

class TideStation {
  const TideStation(
      {required this.id,
      required this.name,
      required this.lat,
      required this.lng});
  final String id;
  final String name;
  final double lat;
  final double lng;
}

class TidePoint {
  const TidePoint({required this.t, required this.v, required this.type});
  final DateTime t;
  final double v;
  final String type; // "H" / "L"
}

class TideSeriesPoint {
  const TideSeriesPoint(this.t, this.v);
  final DateTime t;
  final double v;
}

/// What the drawer shows.
class TideInfo {
  const TideInfo({
    required this.station,
    required this.distNm,
    required this.currentFt,
    required this.nextHigh,
    required this.nextLow,
    required this.series,
    required this.fetchedAt,
  });
  final TideStation station;
  final double distNm;
  final double? currentFt;
  final TidePoint? nextHigh;
  final TidePoint? nextLow;
  final List<TideSeriesPoint> series;
  final DateTime fetchedAt;
}

/// Nearest station by haversine (web uses R = 3440.065 nm).
({TideStation station, double distNm})? nearestTideStation(
    List<TideStation> stations, double lat, double lng) {
  TideStation? best;
  var bestNm = double.infinity;
  const r = 3440.065;
  final lat1 = lat * math.pi / 180;
  for (final s in stations) {
    final lat2 = s.lat * math.pi / 180;
    final dLat = lat2 - lat1;
    final dLng = (s.lng - lng) * math.pi / 180;
    final a = math.pow(math.sin(dLat / 2), 2) +
        math.cos(lat1) * math.cos(lat2) * math.pow(math.sin(dLng / 2), 2);
    final d = r * 2 * math.atan2(math.sqrt(a), math.sqrt(1 - a));
    if (d < bestNm) {
      bestNm = d;
      best = s;
    }
  }
  return best == null ? null : (station: best, distNm: bestNm);
}

/// NOAA's yyyyMMdd in local wall-clock time (time_zone=lst_ldt interprets it
/// station-local; for nearby stations this matches).
String fmtNoaaDate(DateTime d) =>
    '${d.year}${d.month.toString().padLeft(2, '0')}${d.day.toString().padLeft(2, '0')}';

/// Clip [series] to [tStart, tEnd] (ms since epoch), linearly interpolating
/// endpoints so the polyline reaches both window edges.
List<TideSeriesPoint> clipSeries(
    List<TideSeriesPoint> series, int tStart, int tEnd) {
  if (series.isEmpty) return const [];
  final sorted = List.of(series)
    ..sort((a, b) => a.t.compareTo(b.t));
  double? interpAt(int t) {
    if (sorted.length < 2) return null;
    if (t <= sorted.first.t.millisecondsSinceEpoch) return sorted.first.v;
    if (t >= sorted.last.t.millisecondsSinceEpoch) return sorted.last.v;
    for (var i = 1; i < sorted.length; i++) {
      final t1 = sorted[i - 1].t.millisecondsSinceEpoch;
      final t2 = sorted[i].t.millisecondsSinceEpoch;
      if (t >= t1 && t <= t2) {
        final f = (t - t1) / math.max(t2 - t1, 1);
        return sorted[i - 1].v + f * (sorted[i].v - sorted[i - 1].v);
      }
    }
    return null;
  }

  final out = <TideSeriesPoint>[];
  final startV = interpAt(tStart);
  if (startV != null) {
    out.add(TideSeriesPoint(
        DateTime.fromMillisecondsSinceEpoch(tStart), startV));
  }
  for (final p in sorted) {
    final t = p.t.millisecondsSinceEpoch;
    if (t > tStart && t < tEnd) out.add(p);
  }
  final endV = interpAt(tEnd);
  if (endV != null) {
    out.add(TideSeriesPoint(DateTime.fromMillisecondsSinceEpoch(tEnd), endV));
  }
  return out;
}

/// Linear interpolation of the hourly series at [now] for the current level.
double? interpCurrent(List<TideSeriesPoint> series, DateTime now) {
  if (series.isEmpty) return null;
  if (series.length == 1) return series.first.v;
  final t = now.millisecondsSinceEpoch;
  if (t <= series.first.t.millisecondsSinceEpoch) return series.first.v;
  if (t >= series.last.t.millisecondsSinceEpoch) return series.last.v;
  for (var i = 1; i < series.length; i++) {
    final t1 = series[i - 1].t.millisecondsSinceEpoch;
    final t2 = series[i].t.millisecondsSinceEpoch;
    if (t >= t1 && t <= t2) {
      final f = (t - t1) / math.max(t2 - t1, 1);
      return series[i - 1].v + f * (series[i].v - series[i - 1].v);
    }
  }
  return null;
}

/// Synthetic series from hi/lo points via half-cosine interpolation, sampled
/// every 15 min — the fallback when NOAA has no hourly product for a
/// (subordinate) station.
List<TideSeriesPoint> synthSeriesFromHilo(
    List<TidePoint> hilo, int windowStart, int windowEnd) {
  if (hilo.length < 2) return const [];
  final sorted = List.of(hilo)..sort((a, b) => a.t.compareTo(b.t));
  final out = <TideSeriesPoint>[];
  const stepMs = 15 * 60 * 1000;
  for (var t = windowStart; t <= windowEnd; t += stepMs) {
    TidePoint? p1;
    TidePoint? p2;
    for (var i = 0; i < sorted.length - 1; i++) {
      if (sorted[i].t.millisecondsSinceEpoch <= t &&
          t <= sorted[i + 1].t.millisecondsSinceEpoch) {
        p1 = sorted[i];
        p2 = sorted[i + 1];
        break;
      }
    }
    if (p1 == null || p2 == null) continue;
    final f = (t - p1.t.millisecondsSinceEpoch) /
        math.max(p2.t.millisecondsSinceEpoch - p1.t.millisecondsSinceEpoch, 1);
    final mid = (p1.v + p2.v) / 2;
    final half = (p1.v - p2.v) / 2;
    out.add(TideSeriesPoint(DateTime.fromMillisecondsSinceEpoch(t),
        mid + half * math.cos(math.pi * f)));
  }
  return out;
}

/// Nearest-station tide watcher: refreshes when the boat has moved enough to
/// change the answer (or the data goes stale), and lands a [TideInfo] in
/// [BoatState.tide]. Degrades to "no tide info" offline — never a crash.
class TideService {
  TideService(this.state);

  final BoatState state;
  Timer? _timer;
  List<TideStation>? _stations;
  String? _stationId;
  double? _lastLat, _lastLng;
  bool _busy = false;

  /// Station list cache file — static reference data that must survive a
  /// launch (web keeps it in sessionStorage; a phone relaunches more).
  static File? _cacheFile() {
    final home = Platform.environment['HOME'];
    if (home == null || home.isEmpty) return null;
    return File(
        '$home/Library/Application Support/viam-chartplotter/tide-stations.json');
  }

  void start() {
    _timer ??= Timer.periodic(const Duration(seconds: 60), (_) => _refresh());
    unawaited(_refresh());
  }

  Future<void> stop() async {
    _timer?.cancel();
    _timer = null;
  }

  Future<void> _refresh() async {
    if (_busy) return;
    final pos = state.position;
    if (pos == null) return;
    final tide = state.tide;
    final moved = _lastLat == null ||
        (pos.latitude - _lastLat!).abs() > 0.05 ||
        (pos.longitude - _lastLng!).abs() > 0.05;
    final stale = tide == null ||
        DateTime.now().difference(tide.fetchedAt) > const Duration(minutes: 30);
    if (!moved && !stale) return;
    _busy = true;
    try {
      final stations = await _loadStations();
      final nearest =
          nearestTideStation(stations, pos.latitude, pos.longitude);
      if (nearest == null) return;
      _lastLat = pos.latitude;
      _lastLng = pos.longitude;
      if (nearest.station.id == _stationId && !stale) return;
      final predictions = await _fetchPredictions(nearest.station.id);
      final now = DateTime.now();
      final windowStart =
          now.subtract(const Duration(hours: 6)).millisecondsSinceEpoch;
      final windowEnd =
          now.add(const Duration(hours: 18)).millisecondsSinceEpoch;
      var series = predictions.series;
      if (series.isEmpty) {
        // Subordinate stations only publish hi/lo — synthesize.
        series = synthSeriesFromHilo(predictions.hilo, windowStart, windowEnd);
      }
      final clipped = clipSeries(series, windowStart, windowEnd);
      TidePoint? nextOf(String type) {
        for (final p in predictions.hilo) {
          if (p.type == type && p.t.isAfter(now)) return p;
        }
        return null;
      }

      _stationId = nearest.station.id;
      state.setTide(TideInfo(
        station: nearest.station,
        distNm: nearest.distNm,
        currentFt: interpCurrent(clipped, now),
        nextHigh: nextOf('H'),
        nextLow: nextOf('L'),
        series: clipped,
        fetchedAt: now,
      ));
    } catch (_) {
      // NOAA unreachable / offline — keep (or leave absent) the last info.
    } finally {
      _busy = false;
    }
  }

  Future<List<TideStation>> _loadStations() async {
    final cached = _stations;
    if (cached != null && cached.isNotEmpty) return cached;
    // Disk cache first — the list is large and static.
    try {
      final f = _cacheFile();
      if (f != null && f.existsSync()) {
        final parsed = _parseStations(await f.readAsString());
        if (parsed.isNotEmpty) return _stations = parsed;
      }
    } catch (_) {}
    final r = await http
        .get(Uri.parse(
            'https://api.tidesandcurrents.noaa.gov/mdapi/prod/webapi/stations.json?type=tidepredictions'))
        .timeout(const Duration(seconds: 20));
    if (r.statusCode != 200) throw HttpException('stations ${r.statusCode}');
    final parsed = _parseStations(r.body);
    if (parsed.isNotEmpty) {
      _stations = parsed;
      try {
        final f = _cacheFile();
        if (f != null) {
          f.parent.createSync(recursive: true);
          await f.writeAsString(r.body);
        }
      } catch (_) {}
    }
    return parsed;
  }

  static List<TideStation> _parseStations(String body) {
    try {
      final decoded = jsonDecode(body);
      final stations = (decoded as Map)['stations'];
      if (stations is! List) return const [];
      return [
        for (final s in stations)
          if (s is Map && s['id'] != null && s['lat'] is num && s['lng'] is num)
            TideStation(
              id: s['id'].toString(),
              name: (s['name'] ?? '').toString(),
              lat: (s['lat'] as num).toDouble(),
              lng: (s['lng'] as num).toDouble(),
            ),
      ];
    } catch (_) {
      return const [];
    }
  }

  Future<({List<TidePoint> hilo, List<TideSeriesPoint> series})>
      _fetchPredictions(String stationId) async {
    const base = 'https://api.tidesandcurrents.noaa.gov/api/prod/datagetter';
    final common = 'application=viam-chartplotter&station=$stationId'
        '&datum=MLLW&time_zone=lst_ldt&units=english&format=json';
    // 3-day window starting yesterday: points either side of now.
    final begin =
        fmtNoaaDate(DateTime.now().subtract(const Duration(hours: 24)));
    final results = await Future.wait([
      http
          .get(Uri.parse(
              '$base?product=predictions&interval=hilo&begin_date=$begin&range=72&$common'))
          .timeout(const Duration(seconds: 15)),
      http
          .get(Uri.parse(
              '$base?product=predictions&interval=h&begin_date=$begin&range=72&$common'))
          .timeout(const Duration(seconds: 15)),
    ]);
    // NOAA returns 200 with {error:{message}} for many "no data" conditions
    // (e.g. subordinate station + interval=h) — treat as empty, not fatal.
    List<dynamic> predictionsOf(http.Response r) {
      if (r.statusCode != 200) return const [];
      try {
        final d = jsonDecode(r.body);
        final p = (d as Map)['predictions'];
        return p is List ? p : const [];
      } catch (_) {
        return const [];
      }
    }

    DateTime? parseT(dynamic t) =>
        t is String ? DateTime.tryParse(t.replaceFirst(' ', 'T')) : null;

    final hilo = <TidePoint>[];
    for (final p in predictionsOf(results[0])) {
      if (p is! Map) continue;
      final t = parseT(p['t']);
      final v = double.tryParse((p['v'] ?? '').toString());
      if (t != null && v != null) {
        hilo.add(TidePoint(t: t, v: v, type: (p['type'] ?? '').toString()));
      }
    }
    final series = <TideSeriesPoint>[];
    for (final p in predictionsOf(results[1])) {
      if (p is! Map) continue;
      final t = parseT(p['t']);
      final v = double.tryParse((p['v'] ?? '').toString());
      if (t != null && v != null) series.add(TideSeriesPoint(t, v));
    }
    return (hilo: hilo, series: series);
  }
}
