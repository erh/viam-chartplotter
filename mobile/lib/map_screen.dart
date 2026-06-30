import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import 'boat_state.dart';
import 'tile_sources.dart';

/// The whole spike UI: a full-screen map (base-layer switchable) with the
/// boat marker and a translucent data panel. Phone-first; on a wide screen
/// the panel sits in a corner (the responsive split for tablets is a v1
/// concern, stubbed here with a LayoutBuilder breakpoint).
class MapScreen extends StatefulWidget {
  const MapScreen({super.key, required this.state});
  final BoatState state;

  @override
  State<MapScreen> createState() => _MapScreenState();
}

class _MapScreenState extends State<MapScreen> {
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
      body: Stack(
        children: [
          FlutterMap(
            mapController: _map,
            options: const MapOptions(
              initialCenter: LatLng(41.3, -72.0), // Long Island Sound-ish
              initialZoom: 8,
            ),
            children: [
              TileLayer(
                key: ValueKey(_base.id),
                urlTemplate: _base.urlTemplate,
                maxNativeZoom: _base.maxZoom,
                userAgentPackageName: 'com.viam.chartplotter.mobile',
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
          SafeArea(
            child: Padding(
              padding: const EdgeInsets.all(12),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  _DataPanel(state: s),
                  const SizedBox(height: 8),
                  _LayerSwitcher(
                    current: _base,
                    onChanged: (t) => setState(() => _base = t),
                  ),
                ],
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

class _DataPanel extends StatelessWidget {
  const _DataPanel({required this.state});
  final BoatState state;

  String _fmt(double? v, String unit, {int digits = 1}) =>
      v == null ? '—' : '${v.toStringAsFixed(digits)} $unit';

  @override
  Widget build(BuildContext context) {
    final pos = state.position;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 10),
      decoration: BoxDecoration(
        color: Colors.black.withOpacity(0.6),
        borderRadius: BorderRadius.circular(10),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(state.status,
              style: const TextStyle(color: Colors.white70, fontSize: 12)),
          const SizedBox(height: 4),
          Text(
            pos == null
                ? 'Position: —'
                : '${pos.latitude.toStringAsFixed(5)}, ${pos.longitude.toStringAsFixed(5)}',
            style: const TextStyle(
                color: Colors.white,
                fontSize: 16,
                fontWeight: FontWeight.w600),
          ),
          const SizedBox(height: 2),
          Text(
            'SOG ${_fmt(state.speedKn, 'kn')}   '
            'HDG ${_fmt(state.headingDeg, '°', digits: 0)}   '
            'DEPTH ${_fmt(state.depthFt, 'ft')}',
            style: const TextStyle(color: Colors.white, fontSize: 14),
          ),
        ],
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
        color: Colors.black.withOpacity(0.6),
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
