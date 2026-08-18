import 'package:flutter/material.dart';

import 'auth/oauth_config.dart';
import 'boat_state.dart';
import 'config.dart';

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
            _Row('Status', state.status),
            _Row('Last update', state.lastUpdate?.toLocal().toString() ?? '—'),
            _Row('AIS targets', '${state.aisBoats.length}'),
            _Row('Wind', state.windInfo),
            const SizedBox(height: 16),
            const _Section('Discovered sources'),
            if (state.sources.isEmpty)
              const _Row('—', 'not connected yet')
            else
              for (final e in state.sources.entries)
                _Row(e.key, e.value ?? '(none found)'),
            const SizedBox(height: 16),
            const _Section('Config'),
            const _Row('Tile base', Config.tileBase),
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
