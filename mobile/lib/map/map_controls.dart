import 'package:flutter/material.dart';

import '../boat_state.dart';
import '../routes/route_stats.dart';
import '../tile_sources.dart';

/// The chrome that floats over the chart: round toolbar buttons, the base-layer
/// switcher, the connection chip and the navigation ETA pill.
///
/// These live apart from MapScreen so that adding a control doesn't mean
/// editing the same file as the map layers (see ../map_screen.dart).

/// Circular translucent toolbar button. [active] tints it with the primary
/// colour (used for latching toggles like course-up and wind); [busy] swaps the
/// icon for a spinner and disables the tap.
class MapRoundButton extends StatelessWidget {
  const MapRoundButton({
    super.key,
    required this.icon,
    required this.tooltip,
    required this.onTap,
    this.active = false,
    this.busy = false,
  });
  final IconData icon;
  final String tooltip;
  final VoidCallback onTap;
  final bool active;
  final bool busy;

  @override
  Widget build(BuildContext context) {
    return Material(
      color: active
          ? Theme.of(context).colorScheme.primary.withValues(alpha: 0.85)
          : Colors.black.withValues(alpha: 0.6),
      shape: const CircleBorder(),
      child: IconButton(
        icon: busy
            ? const SizedBox(
                width: 18,
                height: 18,
                child: CircularProgressIndicator(
                    strokeWidth: 2, color: Colors.white),
              )
            : Icon(icon, color: Colors.white),
        tooltip: tooltip,
        onPressed: busy ? null : onTap,
      ),
    );
  }
}

/// A [MapRoundButton] that opens a popup menu instead of firing an action —
/// how the toolbar stays a short column of icons instead of a wall of them.
class MapMenuButton<T> extends StatelessWidget {
  const MapMenuButton({
    super.key,
    required this.icon,
    required this.tooltip,
    required this.itemBuilder,
    required this.onSelected,
    this.active = false,
    this.busy = false,
  });
  final IconData icon;
  final String tooltip;
  final List<PopupMenuEntry<T>> Function(BuildContext) itemBuilder;
  final ValueChanged<T> onSelected;
  final bool active;
  final bool busy;

  @override
  Widget build(BuildContext context) {
    return Material(
      color: active
          ? Theme.of(context).colorScheme.primary.withValues(alpha: 0.85)
          : Colors.black.withValues(alpha: 0.6),
      shape: const CircleBorder(),
      child: PopupMenuButton<T>(
        tooltip: tooltip,
        enabled: !busy,
        itemBuilder: itemBuilder,
        onSelected: onSelected,
        icon: busy
            ? const SizedBox(
                width: 18,
                height: 18,
                child: CircularProgressIndicator(
                    strokeWidth: 2, color: Colors.white),
              )
            : Icon(icon, color: Colors.white),
      ),
    );
  }
}

/// Base-layer picker as a single icon (the labeled dropdown ate a corner of
/// the chart); the menu checks the active layer.
class LayerSwitcher extends StatelessWidget {
  const LayerSwitcher({
    super.key,
    required this.current,
    required this.onChanged,
  });
  final TileSource current;
  final ValueChanged<TileSource> onChanged;

  @override
  Widget build(BuildContext context) {
    return MapMenuButton<TileSource>(
      icon: Icons.layers,
      tooltip: 'Base layer (${current.label})',
      itemBuilder: (_) => [
        for (final t in baseLayers)
          CheckedPopupMenuItem(
            value: t,
            checked: t == current,
            child: Text(t.label),
          ),
      ],
      onSelected: onChanged,
    );
  }
}

/// Small pill in the top-left: a colour-coded dot plus the connection status.
/// While disconnected it also carries a retry button, so a helm that's back in
/// range doesn't have to wait out the reconnect backoff.
class StatusChip extends StatelessWidget {
  const StatusChip({super.key, required this.state, this.onReconnect});
  final BoatState state;
  final VoidCallback? onReconnect;

  @override
  Widget build(BuildContext context) {
    final connected = state.connected;
    return Container(
      padding: EdgeInsets.only(
        left: 10,
        right: (connected || onReconnect == null) ? 10 : 2,
        top: 6,
        bottom: 6,
      ),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(20),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Icon(Icons.circle,
              size: 10,
              color: connected ? Colors.greenAccent : Colors.orangeAccent),
          const SizedBox(width: 6),
          ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 220),
            child: Text(
              state.status,
              overflow: TextOverflow.ellipsis,
              style: const TextStyle(color: Colors.white, fontSize: 12),
            ),
          ),
          if (!connected && onReconnect != null)
            IconButton(
              tooltip: 'Reconnect now',
              onPressed: onReconnect,
              icon: const Icon(Icons.refresh, size: 16),
              color: Colors.white,
              visualDensity: VisualDensity.compact,
              padding: const EdgeInsets.symmetric(horizontal: 6),
              constraints: const BoxConstraints(minWidth: 28, minHeight: 28),
            ),
        ],
      ),
    );
  }
}

/// Top-center pill shown while a route is active: distance to the next
/// waypoint and estimated time, from the `route` sensor.
/// Always-visible SOG/COG readout at the top of the chart — the two numbers
/// a helmsman glances at constantly. Dashes until data arrives, so the pill
/// never jumps in and out of the layout.
class SogCogPill extends StatelessWidget {
  const SogCogPill({super.key, required this.state});
  final BoatState state;

  @override
  Widget build(BuildContext context) {
    final sog = state.speedKn;
    final cog = state.cogDeg;
    final sogStr = sog == null ? '–.–' : sog.toStringAsFixed(1);
    final cogStr =
        cog == null ? '–––' : cog.round().toString().padLeft(3, '0');
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 6),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(20),
      ),
      child: Text(
        '$sogStr kn   $cogStr°',
        style: const TextStyle(
          color: Colors.white,
          fontSize: 16,
          fontWeight: FontWeight.w700,
          // Tabular figures: the numbers tick every second and must not
          // make the pill wobble.
          fontFeatures: [FontFeature.tabularFigures()],
        ),
      ),
    );
  }
}

class EtaPill extends StatelessWidget {
  const EtaPill({super.key, required this.state});
  final BoatState state;

  @override
  Widget build(BuildContext context) {
    // Next-waypoint line: the route sensor's live numbers when it has them
    // (closing-velocity ETA, like the web's Next label), else the nav-route
    // stats (SOG-based).
    final stats = state.routeStats;
    final nm = state.wpDistanceNm ?? stats?.nextNm;
    final double? mins = state.wpEtaMinutes ?? stats?.nextMinutes;
    final parts = <String>[
      if (nm != null) '${nm.toStringAsFixed(2)} nm',
      if (nm != null) formatDurationMin(mins), // blank (—), never zero (E5)
    ];
    if (parts.isEmpty && stats == null) return const SizedBox.shrink();
    // Final line (E5): the whole remaining route, only when there's more
    // than one waypoint (web shows Final the same way).
    final showFinal = stats != null && stats.waypointCount > 1;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(20),
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          if (parts.isNotEmpty)
            Row(
              mainAxisSize: MainAxisSize.min,
              children: [
                const Icon(Icons.flag, size: 14, color: Colors.purpleAccent),
                const SizedBox(width: 6),
                Text(
                  parts.join('  ·  '),
                  style: const TextStyle(
                      color: Colors.white,
                      fontSize: 14,
                      fontWeight: FontWeight.w600),
                ),
              ],
            ),
          if (showFinal)
            Text(
              'Final ${stats.finalNm.toStringAsFixed(1)} nm'
              '  ·  ${formatDurationMin(stats.finalMinutes)}'
              '  ·  ETA ${formatEta(stats.finalMinutes)}',
              style: const TextStyle(
                  color: Colors.white70,
                  fontSize: 12,
                  fontWeight: FontWeight.w500),
            ),
        ],
      ),
    );
  }
}
