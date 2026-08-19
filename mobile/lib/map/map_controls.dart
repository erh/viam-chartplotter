import 'package:flutter/material.dart';

import '../boat_state.dart';
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

/// Base-layer picker. Currently a dropdown over [baseLayers]; when the layer
/// count grows past a handful this becomes the layers sheet (task J5).
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
    return Container(
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(10),
      ),
      child: DropdownButtonHideUnderline(
        child: DropdownButton<TileSource>(
          value: current,
          dropdownColor: Colors.black87,
          padding: const EdgeInsets.symmetric(horizontal: 12),
          style: const TextStyle(color: Colors.white),
          items: [
            for (final t in baseLayers)
              DropdownMenuItem(value: t, child: Text(t.label)),
          ],
          onChanged: (t) {
            if (t != null) onChanged(t);
          },
        ),
      ),
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
class EtaPill extends StatelessWidget {
  const EtaPill({super.key, required this.state});
  final BoatState state;

  @override
  Widget build(BuildContext context) {
    final nm = state.wpDistanceNm;
    final mins = state.wpEtaMinutes;
    final parts = <String>[
      if (nm != null) '${nm.toStringAsFixed(2)} nm',
      if (mins != null) '${mins.toStringAsFixed(0)} min',
    ];
    if (parts.isEmpty) return const SizedBox.shrink();
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(20),
      ),
      child: Row(
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
    );
  }
}
