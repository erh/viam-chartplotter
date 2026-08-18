import 'dart:async';

import 'package:flutter/material.dart';

import 'boat_state.dart';
import 'history.dart';

/// Fuel mode: every tank sensor with its level and two freshness ages —
/// when the app last got data from the boat, and when the boat-side sensor
/// last received data on the bus. Under 20 s is green, 20–60 s yellow, over
/// a minute red, so a dead sender or a dropped connection is obvious at a
/// glance. Ages tick every second while the page is open.
class FuelScreen extends StatefulWidget {
  const FuelScreen({super.key, required this.state, this.history});
  final BoatState state;

  /// Cloud tabular backfill (hot store, falling back to cold) for the hour
  /// graphs; null on the API-key path, where graphs are live-only.
  final HistoryService? history;

  @override
  State<FuelScreen> createState() => _FuelScreenState();
}

class _FuelScreenState extends State<FuelScreen> {
  Timer? _ticker;

  @override
  void initState() {
    super.initState();
    widget.state.addListener(_onState);
    _ticker = Timer.periodic(const Duration(seconds: 1), (_) {
      if (mounted) setState(() {});
    });
    _backfill();
  }

  /// Seed each tank's series from the recorded data store (hot → cold), once
  /// per session — after that the live poll keeps the series growing locally.
  Future<void> _backfill() async {
    final h = widget.history;
    if (h == null) return;
    for (final t in List.of(widget.state.tanks)) {
      final key = 'tank:${t.name}';
      if (widget.state.backfilled.contains(key) || !h.hasMetric(key)) continue;
      final pts = await h.fetch(key, const Duration(hours: 1));
      if (!mounted) return;
      widget.state.backfill(key, pts);
    }
  }

  @override
  void dispose() {
    _ticker?.cancel();
    widget.state.removeListener(_onState);
    super.dispose();
  }

  void _onState() {
    if (mounted) setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    final tanks = widget.state.tanks;
    return Scaffold(
      appBar: AppBar(title: const Text('Fuel')),
      body: tanks.isEmpty
          ? const Center(child: Text('No tank sensors found'))
          : ListView.separated(
              padding: const EdgeInsets.all(12),
              itemCount: tanks.length,
              separatorBuilder: (_, __) => const SizedBox(height: 12),
              itemBuilder: (context, i) => _TankCard(
                tank: tanks[i],
                series: widget.state.series('tank:${tanks[i].name}'),
              ),
            ),
    );
  }
}

/// Fill prediction from the last 5 minutes of samples: the level delta over
/// that window, extrapolated to when the tank reaches 95% (the stop-filling
/// heads-up). Null when there's no meaningful upward trend or not enough
/// history yet to trust one (at least half the window).
({double ratePerMin, Duration toTarget})? predict95(
    List<({DateTime t, double v})> series) {
  if (series.length < 2) return null;
  final last = series.last;
  if (last.v >= 95) return null; // already there — the card says so
  final cutoff = last.t.subtract(const Duration(minutes: 5));
  ({DateTime t, double v})? ref;
  for (final s in series) {
    if (!s.t.isBefore(cutoff)) {
      ref = s;
      break;
    }
  }
  if (ref == null) return null;
  final windowMin = last.t.difference(ref.t).inMilliseconds / 60000.0;
  if (windowMin < 2.5) return null; // too little history to extrapolate
  final rate = (last.v - ref.v) / windowMin;
  if (rate < 0.05) return null; // flat or draining — no fill ETA
  final minutes = (95 - last.v) / rate;
  if (minutes > 24 * 60) return null; // absurd horizon → noise, not a fill
  return (
    ratePerMin: rate,
    toTarget: Duration(seconds: (minutes * 60).round()),
  );
}

class _TankCard extends StatelessWidget {
  const _TankCard({required this.tank, required this.series});
  final TankStatus tank;
  final List<({DateTime t, double v})> series;

  @override
  Widget build(BuildContext context) {
    final level = tank.level;
    final capacity = tank.capacity;
    final liters = (level != null && capacity != null)
        ? capacity * level / 100.0
        : null;
    final prediction = predict95(series);
    final now = DateTime.now();
    final hour = [
      for (final s in series)
        if (now.difference(s.t).inSeconds <= 3600) s
    ];
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Expanded(
                  child: Text(tank.name,
                      style: Theme.of(context).textTheme.titleMedium),
                ),
                Text(
                  level == null ? '—' : '${level.toStringAsFixed(0)}%',
                  style: Theme.of(context).textTheme.titleLarge,
                ),
              ],
            ),
            const SizedBox(height: 8),
            ClipRRect(
              borderRadius: BorderRadius.circular(4),
              child: LinearProgressIndicator(
                value: level == null ? 0 : (level / 100).clamp(0.0, 1.0),
                minHeight: 8,
              ),
            ),
            if (liters != null) ...[
              const SizedBox(height: 4),
              Text(
                '${liters.toStringAsFixed(0)} / ${capacity!.toStringAsFixed(0)} L',
                style: Theme.of(context).textTheme.bodySmall,
              ),
            ],
            if (hour.length >= 2) ...[
              const SizedBox(height: 12),
              SizedBox(
                height: 80,
                width: double.infinity,
                child: CustomPaint(painter: _HourGraphPainter(hour, now)),
              ),
            ],
            if (level != null && level >= 95)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  'At ${level.toStringAsFixed(0)}% — above 95%',
                  style: Theme.of(context)
                      .textTheme
                      .bodyMedium
                      ?.copyWith(color: Colors.amber),
                ),
              )
            else if (prediction != null)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  'Filling ${_rateLabel(prediction.ratePerMin, capacity)} → '
                  '95% ≈ ${_clock(now.add(prediction.toTarget))} '
                  '(in ${_dur(prediction.toTarget)})',
                  style: Theme.of(context)
                      .textTheme
                      .bodyMedium
                      ?.copyWith(color: Colors.lightBlueAccent),
                ),
              ),
            const SizedBox(height: 12),
            Row(
              children: [
                Expanded(
                  child:
                      _AgeChip(label: 'App fetch', age: tank.fetchedAge),
                ),
                const SizedBox(width: 8),
                Expanded(
                  child: _AgeChip(
                    label: 'Boat sensor',
                    age: tank.boatAge,
                    invalid: tank.boatTimestampInvalid,
                  ),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}

/// Fill rate for display: gallons/min when the tank's capacity (liters) is
/// known, else the raw percent/min.
String _rateLabel(double pctPerMin, double? capacityL) {
  if (capacityL != null && capacityL > 0) {
    final gpm = pctPerMin / 100.0 * capacityL / 3.78541;
    return '+${gpm.toStringAsFixed(1)} gal/min';
  }
  return '+${pctPerMin.toStringAsFixed(1)}%/min';
}

String _clock(DateTime t) {
  final h = t.hour % 12 == 0 ? 12 : t.hour % 12;
  final m = t.minute.toString().padLeft(2, '0');
  return '$h:$m ${t.hour < 12 ? 'AM' : 'PM'}';
}

String _dur(Duration d) {
  if (d.inMinutes < 1) return '${d.inSeconds}s';
  if (d.inMinutes < 60) return '${d.inMinutes}m';
  return '${d.inHours}h ${d.inMinutes % 60}m';
}

/// Level over the last hour, positioned by real timestamps (a series that
/// only covers 20 minutes occupies the right third). Fixed 0–100% scale so
/// every tank chart reads the same; the 95% target line is drawn dashed in
/// amber.
class _HourGraphPainter extends CustomPainter {
  _HourGraphPainter(this.points, this.now);
  final List<({DateTime t, double v})> points;
  final DateTime now;

  static const double lo = 0;
  static const double hi = 100;

  @override
  void paint(Canvas canvas, Size size) {
    double x(DateTime t) =>
        (1 - now.difference(t).inMilliseconds / 3600000.0) * size.width;
    double y(double v) => size.height - (v - lo) / (hi - lo) * size.height;

    // Frame for orientation.
    final frame = Paint()
      ..color = Colors.white24
      ..style = PaintingStyle.stroke;
    canvas.drawRect(Offset.zero & size, frame);

    // 95% target, dashed.
    {
      final ty = y(95);
      final dash = Paint()
        ..color = Colors.amber
        ..strokeWidth = 1;
      for (double dx = 0; dx < size.width; dx += 8) {
        canvas.drawLine(Offset(dx, ty), Offset(dx + 4, ty), dash);
      }
    }

    final path = Path();
    for (var i = 0; i < points.length; i++) {
      final px = x(points[i].t).clamp(0.0, size.width);
      final py = y(points[i].v);
      if (i == 0) {
        path.moveTo(px, py);
      } else {
        path.lineTo(px, py);
      }
    }
    canvas.drawPath(
      path,
      Paint()
        ..color = Colors.lightBlueAccent
        ..style = PaintingStyle.stroke
        ..strokeWidth = 2
        ..strokeJoin = StrokeJoin.round,
    );

    // Axis labels.
    const style = TextStyle(color: Colors.white54, fontSize: 10);
    for (final (label, align) in [('100%', 0.0), ('0%', size.height - 12)]) {
      final tp = TextPainter(
        text: TextSpan(text: label, style: style),
        textDirection: TextDirection.ltr,
      )..layout();
      tp.paint(canvas, Offset(4, align));
    }
  }

  @override
  bool shouldRepaint(covariant _HourGraphPainter old) =>
      old.points != points || old.now != now;
}

/// Freshness chip: <20 s green, 20–60 s yellow, >60 s red, unknown grey.
/// [invalid] (a garbage/future sensor timestamp) is always red "invalid".
class _AgeChip extends StatelessWidget {
  const _AgeChip({required this.label, required this.age, this.invalid = false});
  final String label;
  final Duration? age;
  final bool invalid;

  Color get _color {
    if (invalid) return Colors.red;
    final a = age;
    if (a == null) return Colors.grey;
    if (a.inSeconds < 20) return Colors.green;
    if (a.inSeconds <= 60) return Colors.amber;
    return Colors.red;
  }

  String get _text {
    if (invalid) return 'invalid';
    final a = age;
    if (a == null) return '—';
    final s = a.inSeconds;
    if (s < 60) return '${s}s ago';
    if (s < 3600) return '${s ~/ 60}m ${s % 60}s ago';
    return '${s ~/ 3600}h ${(s % 3600) ~/ 60}m ago';
  }

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
      decoration: BoxDecoration(
        color: _color.withValues(alpha: 0.15),
        border: Border.all(color: _color),
        borderRadius: BorderRadius.circular(8),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(label, style: Theme.of(context).textTheme.labelSmall),
          const SizedBox(height: 2),
          Text(
            _text,
            style: Theme.of(context)
                .textTheme
                .titleSmall
                ?.copyWith(color: _color, fontWeight: FontWeight.bold),
          ),
        ],
      ),
    );
  }
}
