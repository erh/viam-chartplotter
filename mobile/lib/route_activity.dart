import 'dart:async';
import 'dart:io' show Platform;

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/services.dart';

import 'boat_state.dart';

/// Feeds the iOS lock-screen Live Activity with the active route's numbers
/// (distance + ETA to the next waypoint, and the final destination). While
/// a route is active the activity is updated every [_cadence]; when the
/// route clears (or on dispose / boat switch) it ends.
///
/// iOS suspends the app when the phone locks, so the DISTANCES freeze at
/// their last pushed values (the card says "as of HH:MM") — but the ETA
/// countdowns are ActivityKit timers and keep ticking on the lock screen.
/// No-op off iOS, below iOS 16.1, and on any channel error (e.g. a build
/// without the widget extension).
class RouteActivityService {
  RouteActivityService(this.state) {
    _timer = Timer.periodic(_cadence, (_) => _push());
  }

  static const _cadence = Duration(seconds: 20);
  static const _ch = MethodChannel('chartplotter/route_activity');

  final BoatState state;
  Timer? _timer;
  bool _active = false; // an activity we started is showing

  bool get _supported => !kIsWeb && Platform.isIOS;

  Future<void> _push() async {
    if (!_supported) return;
    double etaEpoch(double? minutes) => minutes == null
        ? 0
        : DateTime.now()
                .add(Duration(seconds: (minutes * 60).round()))
                .millisecondsSinceEpoch /
            1000.0;

    // Nav-service route first; fall back to the route sensor's single
    // destination (an MFD/PGN route) so the lock screen works for both.
    final stats = state.routeStats;
    Map<String, dynamic>? args;
    if (stats != null && state.navWaypoints.isNotEmpty) {
      args = {
        'nextDistNm': stats.nextNm,
        'nextEtaEpoch': etaEpoch(stats.nextMinutes),
        'finalDistNm': stats.finalNm,
        'finalEtaEpoch': etaEpoch(stats.finalMinutes),
        'waypointCount': stats.waypointCount,
        'sogKn': state.speedKn ?? 0,
      };
    } else if (state.navigating && state.wpDistanceNm != null) {
      args = {
        'nextDistNm': state.wpDistanceNm!,
        'nextEtaEpoch': etaEpoch(state.wpEtaMinutes),
        'finalDistNm': state.wpDistanceNm!,
        'finalEtaEpoch': etaEpoch(state.wpEtaMinutes),
        'waypointCount': 1,
        'sogKn': state.speedKn ?? 0,
      };
    }
    if (args == null) {
      if (_active) await _end();
      state.liveActivityInfo = 'no active route';
      return;
    }
    try {
      final status = await _ch.invokeMethod('update', args);
      state.liveActivityInfo = '$status';
      _active = true;
    } catch (e) {
      // Missing extension / old iOS / channel not up — visible in Debug.
      state.liveActivityInfo = 'channel: $e';
    }
  }

  Future<void> _end() async {
    _active = false;
    if (!_supported) return;
    try {
      await _ch.invokeMethod('end');
    } catch (_) {}
  }

  void dispose() {
    _timer?.cancel();
    _timer = null;
    unawaited(_end());
  }
}
