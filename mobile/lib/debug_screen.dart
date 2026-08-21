import 'package:flutter/material.dart';

import 'auth/oauth_config.dart';
import 'app_config.dart';
import 'boat_state.dart';
import 'config.dart';
import 'map/tile_cache.dart';
import 'settings.dart';

/// Developer/diagnostics view — reached by tapping the connection status chip.
/// Shows which component each reading was auto-discovered from, plus connection
/// and config details. Kept out of the everyday data drawer.
class DebugScreen extends StatelessWidget {
  const DebugScreen({super.key, required this.state});
  final BoatState state;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Debug')),
      body: ListenableBuilder(
        listenable: state,
        builder: (context, _) => ListView(
          padding: const EdgeInsets.all(16),
          children: [
            const _Section('Connection'),
            if (state.boatName.isNotEmpty) _Row('Boat', state.boatName),
            _Row('Status', state.status),
            _Row('Last update', state.lastUpdate?.toLocal().toString() ?? '—'),
            if (state.reconnectAttempts > 0)
              _Row('Reconnect tries', '${state.reconnectAttempts}'),
            if (state.lastConnectError != null)
              _Row('Last error', state.lastConnectError!),
            _Row('AIS targets', '${state.aisBoats.length}'),
            if (state.aisCulled > 0 || state.aisCapped > 0)
              _Row('AIS not drawn',
                  '${state.aisCulled} off-screen, ${state.aisCapped} over cap'),
            _Row('Wind', state.windInfo),
            _Row('Live Activity', state.liveActivityInfo),
            // Which binary this is — with a TestFlight install and a dev
            // install side by side, "the fix isn't in" is usually "wrong
            // icon" (dev builds stamp 'dev').
            _Row('Build', Settings.instance.buildStamp),
            const SizedBox(height: 16),
            // Session data budget (L3) — the instrumentation for the
            // "1 hour under way" baseline: read these after a run.
            const _Section('Data this session'),
            _Row('Poll sweeps', '${state.pollSweeps}'),
            _Row('AIS fetches', '${state.aisFetches}'),
            if (TileDiskCache.instance case final c?) ...[
              _Row('Tile downloads',
                  '${c.misses} (${(c.netBytes / (1024 * 1024)).toStringAsFixed(1)} MB)'),
              _Row('Tile cache hits', '${c.hits}'),
            ],
            _Row('Low data mode', Settings.instance.lowDataOn ? 'on' : 'off'),
            const SizedBox(height: 16),
            const _Section('Component health'),
            if (state.resourceHealth.isEmpty)
              const _Row('—', 'all components ready')
            else
              for (final e in state.resourceHealth.entries)
                _Row('⚠ ${e.key}', e.value),
            const SizedBox(height: 16),
            const _Section('Discovered sources'),
            if (state.sources.isEmpty)
              const _Row('—', 'not connected yet')
            else
              for (final e in state.sources.entries)
                _Row(e.key, e.value ?? '(none found)'),
            const SizedBox(height: 16),
            const _Section('Config'),
            _Row('Tile base', AppConfig.tileBase.value),
            _Row('Auth',
                OAuthConfig.configured ? 'app.viam.com login' : 'API key / chart-only'),
            _Row('Host', Config.host.isEmpty ? '—' : Config.host),
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
  const _Row(this.label, this.value);
  final String label;
  final String value;
  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 6),
        child: Row(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            SizedBox(
              width: 110,
              child: Text(label,
                  style: const TextStyle(color: Colors.white60, fontSize: 13)),
            ),
            Expanded(
              child: SelectableText(value,
                  style: const TextStyle(fontSize: 14)),
            ),
          ],
        ),
      );
}
