import 'package:latlong2/latlong.dart';
import 'package:viam_sdk/viam_sdk.dart';

/// How to pull one metric's recorded history out of Viam tabular data: which
/// captured component, which reading field, and how to convert the stored SI
/// value into the app's display unit. Mirrors the web app's per-gauge MQL
/// (src/App.svelte: depthHistoryMQL / seaTempHistoryMQL / getDataViaMQL).
class HistorySpec {
  const HistorySpec(this.component, this.field, this.convert, {this.extraMatch});

  /// Leaf component name (matches tabular data's `component_name`).
  final String component;

  /// Reading key under `data.readings` (e.g. `Depth`, `Level`, `Wind Speed`).
  final String field;

  /// SI-stored value → display unit (m→ft, °C→°F, m/s→kn, …).
  final double Function(double) convert;

  /// Extra `$match` constraints (e.g. wind's ground-referenced Reference).
  final Map<String, dynamic>? extraMatch;
}

/// Backfills the detail graphs from Viam's tabular data store so opening a
/// graph shows real recorded history, not just what the app has sampled live
/// since launch. Only available on the logged-in path (needs the cloud
/// [DataClient] + the machine's cloud identity); degrades to live-only when
/// null.
class HistoryService {
  HistoryService({
    required this.dataClient,
    required this.orgId,
    required this.locationId,
    required this.robotId,
    required Map<String, HistorySpec> specs,
    this.positionComponent,
  }) : _specs = specs;

  final DataClient dataClient;
  final String orgId;
  final String locationId;
  final String robotId;
  final Map<String, HistorySpec> _specs;

  /// Movement-sensor leaf name whose Position captures feed the recorded
  /// track (E3 save-from-track). Null → no track window available.
  final String? positionComponent;

  bool hasMetric(String metric) => _specs.containsKey(metric);

  bool get hasTrackWindow => positionComponent != null && orgId.isNotEmpty;

  /// The recorded track for an explicit [t0, t1] window (web
  /// fetchTrackWindow): bucketed Position captures, chronological. The hot
  /// store only retains ~recent data, so it's only asked when the window's
  /// NEWEST edge is within ~2 days — an older window goes straight to cold —
  /// and an empty hot answer falls back to cold (the window's tail may
  /// already have aged out).
  Future<List<LatLng>> fetchTrackWindow(DateTime t0, DateTime t1) async {
    final comp = positionComponent;
    if (comp == null || orgId.isEmpty) return const [];
    final windowMs = t1.difference(t0).inMilliseconds;
    if (windowMs <= 0) return const [];
    // ~2000 raw fixes across the window; simplify cuts from there.
    final bucketMs = (windowMs / 2000).clamp(1000, 3600000).round();

    final pipeline = <Map<String, dynamic>>[
      {
        r'$match': {
          'location_id': locationId,
          'robot_id': robotId,
          'component_name': comp,
          'method_name': 'Position',
          'time_received': {r'$gte': t0.toUtc(), r'$lte': t1.toUtc()},
        }
      },
      // Newest-first so $first picks the latest fix in each bucket (web).
      {
        r'$sort': {'time_received': -1}
      },
      {
        r'$group': {
          '_id': {
            r'$floor': {
              r'$divide': [
                {r'$toLong': r'$time_received'},
                bucketMs,
              ]
            }
          },
          'ts': {r'$min': r'$time_received'},
          'pos': {r'$first': r'$data'},
        }
      },
      {
        r'$sort': {'ts': 1}
      },
    ];

    final hotEligible =
        t1.isAfter(DateTime.now().subtract(const Duration(days: 2)));
    var rows = hotEligible
        ? await _run(pipeline, hot: true)
        : const <Map<String, dynamic>>[];
    if (rows.isEmpty) rows = await _run(pipeline, hot: false);

    final out = <({DateTime t, LatLng p})>[];
    for (final r in rows) {
      final ts = _asDate(r['ts']);
      final pos = r['pos'];
      final coord = pos is Map ? pos['coordinate'] : null;
      final lat = coord is Map ? coord['latitude'] : null;
      final lng = coord is Map ? coord['longitude'] : null;
      if (ts != null && lat is num && lng is num) {
        out.add((t: ts, p: LatLng(lat.toDouble(), lng.toDouble())));
      }
    }
    out.sort((a, b) => a.t.compareTo(b.t));
    return [for (final e in out) e.p];
  }

  /// Fetch bucketed history for [metric] over the trailing [window], in
  /// chronological order and already unit-converted. Returns an empty list on
  /// any failure or when the metric has no capture spec — callers fall back to
  /// the live in-memory series.
  Future<List<({DateTime t, double v})>> fetch(
      String metric, Duration window) async {
    final spec = _specs[metric];
    if (spec == null || orgId.isEmpty) return const [];

    final start = DateTime.now().toUtc().subtract(window);
    // ~240 points across the window, whatever its length.
    final bucketMs = (window.inMilliseconds / 240).clamp(1000, 3600000).round();
    final fieldPath = '\$data.readings.${spec.field}';

    final match = <String, dynamic>{
      'location_id': locationId,
      'robot_id': robotId,
      'component_name': spec.component,
      'method_name': 'Readings',
      'time_received': {r'$gte': start},
    };
    if (spec.extraMatch != null) match.addAll(spec.extraMatch!);

    final pipeline = <Map<String, dynamic>>[
      {r'$match': match},
      {
        r'$group': {
          '_id': {
            r'$floor': {
              r'$divide': [
                {r'$toLong': r'$time_received'},
                bucketMs,
              ]
            }
          },
          'ts': {r'$min': r'$time_received'},
          'v': {r'$avg': fieldPath},
        }
      },
      {
        r'$sort': {'ts': 1}
      },
    ];

    // Recent windows live in the hot store; older ones only in cold. Try hot
    // first, fall back to cold when it comes back empty (mirrors the web app).
    var rows = await _run(pipeline, hot: true);
    if (rows.isEmpty) rows = await _run(pipeline, hot: false);

    final out = <({DateTime t, double v})>[];
    for (final r in rows) {
      final ts = _asDate(r['ts']);
      final v = r['v'];
      if (ts != null && v is num) {
        out.add((t: ts.toLocal(), v: spec.convert(v.toDouble())));
      }
    }
    out.sort((a, b) => a.t.compareTo(b.t));
    return out;
  }

  Future<List<Map<String, dynamic>>> _run(
      List<Map<String, dynamic>> pipeline,
      {required bool hot}) async {
    try {
      return await dataClient.tabularDataByMql(orgId, pipeline,
          useRecentData: hot);
    } catch (_) {
      return const [];
    }
  }

  static DateTime? _asDate(dynamic v) {
    if (v is DateTime) return v;
    if (v is String) return DateTime.tryParse(v);
    if (v is int) return DateTime.fromMillisecondsSinceEpoch(v, isUtc: true);
    return null;
  }
}
