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
  }) : _specs = specs;

  final DataClient dataClient;
  final String orgId;
  final String locationId;
  final String robotId;
  final Map<String, HistorySpec> _specs;

  bool hasMetric(String metric) => _specs.containsKey(metric);

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
