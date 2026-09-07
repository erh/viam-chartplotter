import 'package:flutter/material.dart';
import 'package:latlong2/latlong.dart';

import 'auto_route.dart';

/// Auto-route bottom sheet: plans a safe route from [start] to [end] on the
/// chart server the moment it opens, previews it on the chart behind the
/// sheet (via [onPreview]), and shows the skipper the cautions before
/// anything is loaded. Pops with the planned waypoints when the user hits
/// "Load route", null otherwise — the caller loads them into the nav
/// service and clears the preview either way.
///
/// The touch design differs from the web's Routes-panel form on purpose:
/// on the phone the destination was just tapped or searched, so there is
/// nothing to fill in — plan immediately, show the result.
class AutoRouteSheet extends StatefulWidget {
  const AutoRouteSheet({
    super.key,
    required this.base,
    required this.start,
    required this.end,
    this.safeDepthFt,
    required this.onPreview,
  });

  final String base;
  final LatLng start;
  final LatLng end;

  /// The operator's configured safe depth (A2), the same number that drives
  /// the chart shading; null lets the server fall back to the boat's draft.
  final double? safeDepthFt;

  /// Draws (and redraws) the candidate polyline on the chart behind the
  /// sheet; called with the waypoints once planning succeeds.
  final void Function(List<LatLng> waypoints) onPreview;

  @override
  State<AutoRouteSheet> createState() => _AutoRouteSheetState();
}

class _AutoRouteSheetState extends State<AutoRouteSheet> {
  AutoRouteResult? _result;
  String? _error;

  @override
  void initState() {
    super.initState();
    _plan();
  }

  Future<void> _plan() async {
    setState(() {
      _result = null;
      _error = null;
    });
    try {
      final res = await planAutoRoute(
        widget.base,
        start: widget.start,
        end: widget.end,
        safeDepthFt: widget.safeDepthFt,
      );
      if (!mounted) return;
      setState(() => _result = res);
      widget.onPreview(res.waypoints);
    } catch (e) {
      if (!mounted) return;
      // The server's message names the depth that made it impossible —
      // show it verbatim.
      setState(() =>
          _error = e.toString().replaceFirst('Exception: ', ''));
    }
  }

  @override
  Widget build(BuildContext context) {
    final res = _result;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(20, 0, 20, 12),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(children: [
              const Icon(Icons.alt_route, color: Colors.lightGreenAccent),
              const SizedBox(width: 10),
              Text('Auto route',
                  style: Theme.of(context).textTheme.titleLarge),
            ]),
            const SizedBox(height: 10),
            if (_error case final err?) ...[
              Text(err, style: const TextStyle(color: Colors.redAccent)),
              const SizedBox(height: 8),
              Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  TextButton(
                      onPressed: () => Navigator.pop(context),
                      child: const Text('Close')),
                  TextButton(
                      onPressed: _plan, child: const Text('Try again')),
                ],
              ),
            ] else if (res == null) ...[
              const Row(children: [
                SizedBox(
                    width: 18,
                    height: 18,
                    child: CircularProgressIndicator(strokeWidth: 2)),
                SizedBox(width: 12),
                Text('Planning a route that stays off land and shoals…'),
              ]),
              const SizedBox(height: 8),
            ] else ...[
              Text(
                '${metresToNm(res.distanceMeters).toStringAsFixed(1)} nm'
                ' (direct ${metresToNm(res.directMeters).toStringAsFixed(1)} nm)'
                ' · ${res.waypoints.length} waypoints',
                style: const TextStyle(
                    fontSize: 16, fontWeight: FontWeight.w600),
              ),
              const SizedBox(height: 6),
              for (final caution in routeCautions(res))
                Padding(
                  padding: const EdgeInsets.symmetric(vertical: 2),
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      const Icon(Icons.warning_amber,
                          size: 16, color: Colors.orangeAccent),
                      const SizedBox(width: 6),
                      Expanded(
                          child: Text(caution,
                              style: const TextStyle(fontSize: 13))),
                    ],
                  ),
                ),
              const SizedBox(height: 8),
              Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  TextButton(
                      onPressed: () => Navigator.pop(context),
                      child: const Text('Cancel')),
                  const SizedBox(width: 8),
                  FilledButton.icon(
                    icon: const Icon(Icons.sailing),
                    label: const Text('Load route'),
                    onPressed: res.waypoints.length >= 2
                        ? () => Navigator.pop(context, res.waypoints)
                        : null,
                  ),
                ],
              ),
            ],
          ],
        ),
      ),
    );
  }
}
