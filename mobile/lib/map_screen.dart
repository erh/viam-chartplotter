import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import 'ais.dart';
import 'boat_state.dart';
import 'camera_screen.dart';
import 'config.dart';
import 'data_drawer.dart';
import 'debug_screen.dart';
import 'tile_sources.dart';
import 'viam_connection.dart';
import 'weather.dart';
import 'wind_layer.dart';

/// Full-screen chart with a heading-rotated boat marker. The data readouts live
/// in a dashboard drawer (DataDrawer) rather than overlaid on the chart; only
/// map *controls* (layer switcher, dashboard button, recenter) sit on top.
class MapScreen extends StatefulWidget {
  const MapScreen({super.key, required this.state, required this.connection});
  final BoatState state;
  final ViamConnection connection;

  @override
  State<MapScreen> createState() => _MapScreenState();
}

class _MapScreenState extends State<MapScreen> {
  final GlobalKey<ScaffoldState> _scaffoldKey = GlobalKey<ScaffoldState>();
  final MapController _map = MapController();
  TileSource _base = baseLayers.first;
  bool _followedFirstFix = false;

  // Wind overlay (fetched lazily on first toggle-on).
  WindField? _wind;
  bool _windOn = false;
  bool _windLoading = false;

  Future<void> _toggleWind() async {
    if (_windOn) {
      setState(() => _windOn = false);
      return;
    }
    if (_wind != null) {
      setState(() => _windOn = true);
      return;
    }
    setState(() => _windLoading = true);
    widget.state
        .setWindInfo('fetching ${Config.tileBase}/noaa-weather/data/gfs/…');
    try {
      final f = await fetchWindField(Config.tileBase, 'gfs');
      if (!mounted) return;
      setState(() {
        _wind = f;
        _windOn = true;
        _windLoading = false;
      });
      widget.state
          .setWindInfo('loaded ${f.nx}×${f.ny} grid (${f.u.length} cells)');
    } catch (e) {
      if (!mounted) return;
      setState(() => _windLoading = false);
      widget.state.setWindInfo('error: $e');
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text('Wind unavailable: $e')));
    }
  }

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

  void _showAisDetails(AisBoat b) {
    showModalBottomSheet<void>(
      context: context,
      showDragHandle: true,
      builder: (ctx) => SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(20, 0, 20, 20),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(b.displayName, style: Theme.of(ctx).textTheme.titleLarge),
              const SizedBox(height: 2),
              Text('MMSI ${b.mmsi}',
                  style: const TextStyle(color: Colors.white54, fontSize: 12)),
              const SizedBox(height: 12),
              _aisRow('SOG', '${b.sogKn.toStringAsFixed(1)} kn'),
              _aisRow('COG',
                  b.cogDeg == null ? '—' : '${b.cogDeg!.toStringAsFixed(0)}°'),
              _aisRow(
                  'Heading',
                  b.headingDeg == null
                      ? '—'
                      : '${b.headingDeg!.toStringAsFixed(0)}°'),
              if (b.lengthM != null)
                _aisRow('Length', '${b.lengthM!.toStringAsFixed(0)} m'),
              if (b.beamM != null)
                _aisRow('Beam', '${b.beamM!.toStringAsFixed(0)} m'),
              if (b.destination != null) _aisRow('Destination', b.destination!),
              _aisRow(
                  'Position',
                  '${b.location.latitude.toStringAsFixed(5)}, '
                      '${b.location.longitude.toStringAsFixed(5)}'),
            ],
          ),
        ),
      ),
    );
  }

  Widget _aisRow(String k, String v) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 4),
        child: Row(
          children: [
            SizedBox(
              width: 96,
              child: Text(k,
                  style: const TextStyle(color: Colors.white60, fontSize: 13)),
            ),
            Expanded(
              child: Text(v,
                  style: const TextStyle(
                      fontSize: 15, fontWeight: FontWeight.w600)),
            ),
          ],
        ),
      );

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
              // Wind overlay (over the chart, under markers).
              if (_windOn && _wind != null) WindLayer(field: _wind!),
              // Active route: line from the boat to the destination.
              if (s.position != null && s.destination != null)
                PolylineLayer(
                  polylines: [
                    Polyline(
                      points: [s.position!, s.destination!],
                      strokeWidth: 3,
                      color: Colors.purpleAccent,
                    ),
                  ],
                ),
              if (s.destination != null)
                MarkerLayer(
                  markers: [
                    Marker(
                      point: s.destination!,
                      width: 30,
                      height: 30,
                      child: const Icon(Icons.flag,
                          color: Colors.purpleAccent, size: 26),
                    ),
                  ],
                ),
              // AIS targets (drawn under the own-boat marker).
              if (s.aisBoats.isNotEmpty)
                MarkerLayer(
                  markers: [
                    for (final b in s.aisBoats)
                      Marker(
                        point: b.location,
                        width: 30,
                        height: 30,
                        child: GestureDetector(
                          onTap: () => _showAisDetails(b),
                          child: Transform.rotate(
                            angle: b.orientationDeg * math.pi / 180.0,
                            child: const Icon(Icons.navigation,
                                color: Colors.cyanAccent, size: 22),
                          ),
                        ),
                      ),
                  ],
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
                child: GestureDetector(
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => DebugScreen(state: s),
                    ),
                  ),
                  child: _StatusChip(state: s),
                ),
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
                    const SizedBox(height: 8),
                    _RoundButton(
                      icon: Icons.air,
                      tooltip: 'Wind',
                      active: _windOn,
                      busy: _windLoading,
                      onTap: _toggleWind,
                    ),
                    if (s.cameraNames.isNotEmpty &&
                        widget.connection.robot != null) ...[
                      const SizedBox(height: 8),
                      _RoundButton(
                        icon: Icons.videocam,
                        tooltip: 'Cameras',
                        onTap: () => Navigator.of(context).push(
                          MaterialPageRoute<void>(
                            builder: (_) => CameraScreen(
                              robot: widget.connection.robot!,
                              names: s.cameraNames,
                            ),
                          ),
                        ),
                      ),
                    ],
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
  const _RoundButton({
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
