import 'dart:async';

import 'package:flutter/material.dart';

import 'boat_state.dart';

/// Fuel mode: every tank sensor with its level and two freshness ages —
/// when the app last got data from the boat, and when the boat-side sensor
/// last received data on the bus. Under 20 s is green, 20–60 s yellow, over
/// a minute red, so a dead sender or a dropped connection is obvious at a
/// glance. Ages tick every second while the page is open.
class FuelScreen extends StatefulWidget {
  const FuelScreen({super.key, required this.state});
  final BoatState state;

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
              itemBuilder: (context, i) => _TankCard(tank: tanks[i]),
            ),
    );
  }
}

class _TankCard extends StatelessWidget {
  const _TankCard({required this.tank});
  final TankStatus tank;

  @override
  Widget build(BuildContext context) {
    final level = tank.level;
    final capacity = tank.capacity;
    final liters = (level != null && capacity != null)
        ? capacity * level / 100.0
        : null;
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
