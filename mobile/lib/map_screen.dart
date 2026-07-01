import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import 'boat_state.dart';
import 'data_drawer.dart';
import 'tile_sources.dart';

/// Full-screen chart with a heading-rotated boat marker. The data readouts live
/// in a dashboard drawer (DataDrawer) rather than overlaid on the chart; only
/// map *controls* (layer switcher, dashboard button, recenter) sit on top.
class MapScreen extends StatefulWidget {
  const MapScreen({super.key, required this.state});
  final BoatState state;

  @override
  State<MapScreen> createState() => _MapScreenState();
}

class _MapScreenState extends State<MapScreen> {
  final GlobalKey<ScaffoldState> _scaffoldKey = GlobalKey<ScaffoldState>();
  final MapController _map = MapController();
  TileSource _base = baseLayers.first;
  bool _followedFirstFix = false;

  @override
  void initState() {
    super.initState();
    widget.state.addListener(_onState);
  }

  @override
  void dispose() {
    widget.state.removeListener(_onState);
    super.dispose();
  }

  void _onState() {
    // Recenter once when the first GPS fix arrives, then leave the user in
    // control of the viewport.
    final pos = widget.state.position;
    if (!_followedFirstFix && pos != null) {
      _followedFirstFix = true;
      _map.move(pos, 13);
    }
    if (mounted) setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    final s = widget.state;
    return Scaffold(
      key: _scaffoldKey,
      endDrawer: DataDrawer(state: s),
      body: Stack(
        children: [
          FlutterMap(
            mapController: _map,
            options: const MapOptions(
              initialCenter: LatLng(41.3, -72.0), // Long Island Sound-ish
              initialZoom: 9,
            ),
            children: [
              TileLayer(
                key: ValueKey(_base.id),
                urlTemplate: _base.urlTemplate,
                minZoom: _base.minZoom.toDouble(),
                maxNativeZoom: _base.maxZoom,
                userAgentPackageName: 'com.viam.chartplotter.mobile',
                // flutter_map shows nothing for a failed tile, so a broken
                // URL/host reads as a blank map — log it instead.
                errorTileCallback: (tile, error, stackTrace) =>
                    debugPrint('tile load failed (${_base.id}): $error'),
              ),
              if (s.position != null)
                MarkerLayer(
                  markers: [
                    Marker(
                      point: s.position!,
                      width: 40,
                      height: 40,
                      child: _BoatMarker(headingDeg: s.headingDeg ?? 0),
                    ),
                  ],
                ),
            ],
          ),
          // Top-left: compact connection status.
          SafeArea(
            child: Align(
              alignment: Alignment.topLeft,
              child: Padding(
                padding: const EdgeInsets.all(12),
                child: _StatusChip(state: s),
              ),
            ),
          ),
          // Top-right: map controls (layer switcher + dashboard opener).
          SafeArea(
            child: Align(
              alignment: Alignment.topRight,
              child: Padding(
                padding: const EdgeInsets.all(12),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.end,
                  children: [
                    _LayerSwitcher(
                      current: _base,
                      onChanged: (t) => setState(() => _base = t),
                    ),
                    const SizedBox(height: 8),
                    _RoundButton(
                      icon: Icons.dashboard,
                      tooltip: 'Boat data',
                      onTap: () => _scaffoldKey.currentState?.openEndDrawer(),
                    ),
                  ],
                ),
              ),
            ),
          ),
        ],
      ),
      floatingActionButton: s.position == null
          ? null
          : FloatingActionButton(
              onPressed: () => _map.move(s.position!, 14),
              tooltip: 'Center on boat',
              child: const Icon(Icons.my_location),
            ),
    );
  }
}

class _BoatMarker extends StatelessWidget {
  const _BoatMarker({required this.headingDeg});
  final double headingDeg;

  @override
  Widget build(BuildContext context) {
    return Transform.rotate(
      angle: headingDeg * math.pi / 180.0,
      child: const Icon(Icons.navigation, color: Colors.red, size: 36),
    );
  }
}

/// Small pill in the top-left: a colour-coded dot plus the connection status.
class _StatusChip extends StatelessWidget {
  const _StatusChip({required this.state});
  final BoatState state;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(20),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Icon(Icons.circle,
              size: 10,
              color:
                  state.connected ? Colors.greenAccent : Colors.orangeAccent),
          const SizedBox(width: 6),
          ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 220),
            child: Text(
              state.status,
              overflow: TextOverflow.ellipsis,
              style: const TextStyle(color: Colors.white, fontSize: 12),
            ),
          ),
        ],
      ),
    );
  }
}

class _RoundButton extends StatelessWidget {
  const _RoundButton(
      {required this.icon, required this.tooltip, required this.onTap});
  final IconData icon;
  final String tooltip;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return Material(
      color: Colors.black.withValues(alpha: 0.6),
      shape: const CircleBorder(),
      child: IconButton(
        icon: Icon(icon, color: Colors.white),
        tooltip: tooltip,
        onPressed: onTap,
      ),
    );
  }
}

class _LayerSwitcher extends StatelessWidget {
  const _LayerSwitcher({required this.current, required this.onChanged});
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
