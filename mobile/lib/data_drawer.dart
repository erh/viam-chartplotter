import 'package:flutter/material.dart';

import 'boat_state.dart';
import 'sparkline.dart';

/// The dashboard: all the boat readouts that used to be overlaid on the chart,
/// grouped into sections. Opened from the map's dashboard button. Rebuilds live
/// while open because MapScreen setState()s on every BoatState tick.
class DataDrawer extends StatelessWidget {
  const DataDrawer({super.key, required this.state});
  final BoatState state;

  String _fmt(double? v, String unit, {int digits = 1}) =>
      v == null ? '—' : '${v.toStringAsFixed(digits)} $unit';

  String _seakeeper(BoatState s) {
    if (s.seakeeperStabilizing == true) {
      final p = s.seakeeperProgress;
      if (p != null && p < 100) return 'Spooling ${p.toStringAsFixed(0)}%';
      return 'Stabilizing';
    }
    if (s.seakeeperPower == true) return 'On (idle)';
    return 'Off';
  }

  @override
  Widget build(BuildContext context) {
    final pos = state.position;
    final wind = state.windSpeedKn == null
        ? '—'
        : '${state.windSpeedKn!.toStringAsFixed(1)} kn @ '
            '${(state.windAngleDeg ?? 0).toStringAsFixed(0)}°';
    return Drawer(
      child: SafeArea(
        child: ListView(
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
          children: [
            Text('Boat data', style: Theme.of(context).textTheme.titleLarge),
            const SizedBox(height: 4),
            Row(
              children: [
                Icon(Icons.circle,
                    size: 10,
                    color: state.connected
                        ? Colors.greenAccent
                        : Colors.orangeAccent),
                const SizedBox(width: 6),
                Expanded(
                  child: Text(state.status,
                      style: const TextStyle(
                          color: Colors.white70, fontSize: 12)),
                ),
              ],
            ),
            const SizedBox(height: 16),
            const _Section('Navigation'),
            _Row(
              'Position',
              pos == null
                  ? '—'
                  : '${pos.latitude.toStringAsFixed(5)}, '
                      '${pos.longitude.toStringAsFixed(5)}',
            ),
            _Row('SOG', _fmt(state.speedKn, 'kn'), spark: state.spark('sog')),
            _Row('COG', _fmt(state.cogDeg, '°', digits: 0)),
            _Row('Heading', _fmt(state.headingDeg, '°', digits: 0)),
            const SizedBox(height: 16),
            const _Section('Environment'),
            _Row('Depth', _fmt(state.depthFt, 'ft'),
                spark: state.spark('depth')),
            _Row('Water temp', _fmt(state.seaTempF, '°F'),
                spark: state.spark('seatemp')),
            _Row('Wind', wind, spark: state.spark('wind')),
            if (state.spotZeroFwGph != null || state.spotZeroSwGph != null) ...[
              const SizedBox(height: 16),
              const _Section('Watermaker'),
              if (state.spotZeroFwGph != null)
                _Row('Fresh water', _fmt(state.spotZeroFwGph, 'gph')),
              if (state.spotZeroSwGph != null)
                _Row('Sea water', _fmt(state.spotZeroSwGph, 'gph')),
            ],
            if (state.tanks.isNotEmpty) ...[
              const SizedBox(height: 16),
              const _Section('Tanks'),
              for (final t in state.tanks)
                _Row(t.name, '${t.level.toStringAsFixed(0)}%',
                    spark: state.spark('tank:${t.name}')),
            ],
            if (state.seakeeperStabilizing != null ||
                state.acVolts != null) ...[
              const SizedBox(height: 16),
              const _Section('Systems'),
              if (state.seakeeperStabilizing != null)
                _Row('Seakeeper', _seakeeper(state)),
              if (state.acVolts != null)
                _Row(
                    'AC power',
                    '${state.acWatts?.toStringAsFixed(0) ?? "—"} W '
                        '@ ${state.acVolts!.toStringAsFixed(0)} V'),
            ],
          ],
        ),
      ),
    );
  }
}

class _Section extends StatelessWidget {
  const _Section(this.title);
  final String title;
  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.only(bottom: 6),
        child: Text(title.toUpperCase(),
            style: const TextStyle(
                color: Colors.white54,
                fontSize: 11,
                fontWeight: FontWeight.w700,
                letterSpacing: 0.5)),
      );
}

class _Row extends StatelessWidget {
  const _Row(this.label, this.value, {this.spark});
  final String label;
  final String value;
  final List<double>? spark;
  @override
  Widget build(BuildContext context) {
    final s = spark;
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 6),
      child: Row(
        children: [
          SizedBox(
            width: 96,
            child: Text(label,
                style: const TextStyle(color: Colors.white60, fontSize: 13)),
          ),
          Expanded(
            child: Text(value,
                style: const TextStyle(
                    color: Colors.white,
                    fontSize: 16,
                    fontWeight: FontWeight.w600)),
          ),
          if (s != null && s.length >= 2) Sparkline(data: s),
        ],
      ),
    );
  }
}
