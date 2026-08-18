import 'dart:math' show max, min;

import 'package:flutter/material.dart';

import 'boat_state.dart';
import 'history.dart';

/// Full-screen detail graph for one metric with a time x-axis and an
/// adjustable time window (15 min / 1 h / 4 h / all). Opened by tapping a row
/// in the data drawer. On open it backfills recorded history from Viam's
/// tabular data store (via [history]) and then live-updates as new samples
/// arrive. The 4 h window is the "fueling" case: a tight, highly detailed view.
class GraphScreen extends StatefulWidget {
  const GraphScreen({
    super.key,
    required this.state,
    required this.metric,
    required this.label,
    required this.unit,
    this.digits = 1,
    this.history,
  });

  final BoatState state;
  final String metric;
  final String label;
  final String unit;
  final int digits;
  final HistoryService? history;

  @override
  State<GraphScreen> createState() => _GraphScreenState();
}

class _GraphScreenState extends State<GraphScreen> {
  // For the "All" window we still bound the backfill to 24 h.
  static const _backfillCap = Duration(hours: 24);
  static const _windows = <(String, Duration?)>[
    ('15m', Duration(minutes: 15)),
    ('1h', Duration(hours: 1)),
    ('4h', Duration(hours: 4)),
    ('All', null),
  ];
  int _sel = 2; // default to the 4 h "fueling" window

  List<({DateTime t, double v})> _backfill = const [];
  bool _loading = false;
  int _fetchId = 0; // guards against out-of-order responses

  @override
  void initState() {
    super.initState();
    _loadHistory();
  }

  bool get _canBackfill =>
      widget.history?.hasMetric(widget.metric) ?? false;

  Future<void> _loadHistory() async {
    final h = widget.history;
    if (h == null || !h.hasMetric(widget.metric)) return;
    final id = ++_fetchId;
    setState(() => _loading = true);
    final rows =
        await h.fetch(widget.metric, _windows[_sel].$2 ?? _backfillCap);
    if (!mounted || id != _fetchId) return;
    setState(() {
      _backfill = rows;
      _loading = false;
    });
  }

  void _selectWindow(int i) {
    setState(() => _sel = i);
    _loadHistory();
  }

  @override
  Widget build(BuildContext context) {
    final window = _windows[_sel].$2;
    return Scaffold(
      appBar: AppBar(
        title: Text(widget.label),
        actions: [
          if (_canBackfill)
            IconButton(
              tooltip: 'Refresh history',
              onPressed: _loading ? null : _loadHistory,
              icon: const Icon(Icons.refresh),
            ),
        ],
      ),
      body: Column(
        children: [
          Padding(
            padding: const EdgeInsets.all(12),
            child: Row(
              children: [
                Wrap(
                  spacing: 8,
                  children: [
                    for (var i = 0; i < _windows.length; i++)
                      ChoiceChip(
                        label: Text(_windows[i].$1),
                        selected: _sel == i,
                        onSelected: (_) => _selectWindow(i),
                      ),
                  ],
                ),
                if (_loading) ...[
                  const SizedBox(width: 12),
                  const SizedBox(
                      width: 16,
                      height: 16,
                      child: CircularProgressIndicator(strokeWidth: 2)),
                ],
              ],
            ),
          ),
          Expanded(
            child: ListenableBuilder(
              listenable: widget.state,
              builder: (context, _) {
                final now = DateTime.now();
                final live = widget.state.series(widget.metric);
                final data = _merge(live, window, now);
                if (data.length < 2) {
                  return Center(
                    child: Text(
                        _loading ? 'Loading history…' : 'Collecting data…',
                        style: const TextStyle(color: Colors.white54)),
                  );
                }
                return Padding(
                  padding: const EdgeInsets.fromLTRB(12, 8, 20, 16),
                  child: CustomPaint(
                    size: Size.infinite,
                    painter: _GraphPainter(
                      data: data,
                      unit: widget.unit,
                      digits: widget.digits,
                      windowEnd: now,
                      windowStart: window == null
                          ? data.first.t
                          : now.subtract(window),
                    ),
                  ),
                );
              },
            ),
          ),
        ],
      ),
    );
  }

  /// Backfill (older, cloud-recorded) points spliced in front of the live
  /// in-memory series, both clipped to the window. The live series carries the
  /// recent, high-resolution tail; backfill covers everything before it.
  List<({DateTime t, double v})> _merge(
      List<({DateTime t, double v})> live, Duration? window, DateTime now) {
    final liveIn = window == null
        ? live
        : [
            for (final s in live)
              if (now.difference(s.t) <= window) s
          ];
    final liveStart = liveIn.isNotEmpty ? liveIn.first.t : now;
    final back = [
      for (final b in _backfill)
        if (b.t.isBefore(liveStart) &&
            (window == null || now.difference(b.t) <= window))
          b
    ];
    return [...back, ...liveIn];
  }
}

class _GraphPainter extends CustomPainter {
  _GraphPainter({
    required this.data,
    required this.unit,
    required this.digits,
    required this.windowStart,
    required this.windowEnd,
  });

  final List<({DateTime t, double v})> data;
  final String unit;
  final int digits;
  final DateTime windowStart;
  final DateTime windowEnd;

  @override
  void paint(Canvas canvas, Size size) {
    const leftPad = 48.0;
    const bottomPad = 24.0;
    final plotW = size.width - leftPad;
    final plotH = size.height - bottomPad;

    var lo = data.map((e) => e.v).reduce(min);
    var hi = data.map((e) => e.v).reduce(max);
    if (hi - lo < 1e-9) {
      hi = lo + 1;
      lo = lo - 1;
    } else {
      final pad = (hi - lo) * 0.08;
      lo -= pad;
      hi += pad;
    }

    final startMs = windowStart.millisecondsSinceEpoch.toDouble();
    final endMs = windowEnd.millisecondsSinceEpoch.toDouble();
    final spanMs = (endMs - startMs).clamp(1.0, double.infinity);

    double xOf(DateTime t) =>
        leftPad + (t.millisecondsSinceEpoch - startMs) / spanMs * plotW;
    double yOf(double v) => plotH - (v - lo) / (hi - lo) * plotH;

    final axis = Paint()
      ..color = Colors.white24
      ..strokeWidth = 1;
    final grid = Paint()
      ..color = Colors.white12
      ..strokeWidth = 1;

    // Horizontal gridlines + y labels.
    const yTicks = 4;
    for (var i = 0; i <= yTicks; i++) {
      final v = lo + (hi - lo) * i / yTicks;
      final y = yOf(v);
      canvas.drawLine(Offset(leftPad, y), Offset(size.width, y), grid);
      _label(canvas, v.toStringAsFixed(digits), Offset(4, y - 6),
          color: Colors.white54);
    }

    // Axes.
    canvas.drawLine(
        const Offset(leftPad, 0), Offset(leftPad, plotH), axis);
    canvas.drawLine(
        Offset(leftPad, plotH), Offset(size.width, plotH), axis);

    // Vertical gridlines + time labels.
    const xTicks = 4;
    for (var i = 0; i <= xTicks; i++) {
      final ms = startMs + spanMs * i / xTicks;
      final t = DateTime.fromMillisecondsSinceEpoch(ms.round());
      final x = leftPad + plotW * i / xTicks;
      canvas.drawLine(Offset(x, 0), Offset(x, plotH), grid);
      final hh = t.hour.toString().padLeft(2, '0');
      final mm = t.minute.toString().padLeft(2, '0');
      _label(canvas, '$hh:$mm', Offset(x - 16, plotH + 4),
          color: Colors.white54);
    }

    // The trend line.
    final path = Path();
    for (var i = 0; i < data.length; i++) {
      final x = xOf(data[i].t);
      final y = yOf(data[i].v);
      if (i == 0) {
        path.moveTo(x, y);
      } else {
        path.lineTo(x, y);
      }
    }
    canvas.drawPath(
      path,
      Paint()
        ..color = Colors.cyanAccent
        ..style = PaintingStyle.stroke
        ..strokeWidth = 2
        ..strokeJoin = StrokeJoin.round,
    );

    // Latest value dot + readout.
    final last = data.last;
    canvas.drawCircle(
        Offset(xOf(last.t), yOf(last.v)), 3, Paint()..color = Colors.cyanAccent);
    _label(canvas, '${last.v.toStringAsFixed(digits)} $unit',
        const Offset(leftPad + 4, 4),
        color: Colors.cyanAccent, size: 13);
  }

  void _label(Canvas canvas, String text, Offset at,
      {required Color color, double size = 10}) {
    final tp = TextPainter(
      text: TextSpan(
          text: text, style: TextStyle(color: color, fontSize: size)),
      textDirection: TextDirection.ltr,
    )..layout();
    tp.paint(canvas, at);
  }

  @override
  bool shouldRepaint(covariant _GraphPainter old) =>
      old.data != data || old.windowStart != windowStart;
}
