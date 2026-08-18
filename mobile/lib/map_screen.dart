import 'package:flutter/foundation.dart'
    show defaultTargetPlatform, kIsWeb, TargetPlatform;
import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import 'boat_state.dart';
import 'camera_screen.dart';
import 'data_drawer.dart';
import 'debug_screen.dart';
import 'fuel_screen.dart';
import 'map/ais_sheet.dart';
import 'map/map_controls.dart';
import 'map/map_layers.dart';
import 'map/wind_overlay.dart';
import 'tile_sources.dart';
import 'viam_connection.dart';

/// Full-screen chart with a heading-rotated boat marker. The data readouts live
/// in a dashboard drawer (DataDrawer) rather than overlaid on the chart; only
/// map *controls* (layer switcher, dashboard button, recenter) sit on top.
///
/// This file owns the screen's *composition and state*. The pieces live beside
/// it so parallel work doesn't collide in one file:
///   map/map_layers.dart    — the FlutterMap child stack (chart, AIS, boat)
///   map/map_controls.dart  — the floating buttons, layer switcher, chips
///   map/wind_overlay.dart  — the wind field, its markers and forecast slider
///   map/ais_sheet.dart     — the AIS target detail sheet
///   map/boat_marker.dart   — the own-boat marker
class MapScreen extends StatefulWidget {
  const MapScreen({
    super.key,
    required this.state,
    required this.connection,
    this.onSwitchBoat,
  });
  final BoatState state;
  final ViamConnection connection;

  /// Disconnect and return to the machine picker. Null on the API-key /
  /// chart-only path, where there is no boat list to go back to.
  final VoidCallback? onSwitchBoat;

  @override
  State<MapScreen> createState() => _MapScreenState();
}

class _MapScreenState extends State<MapScreen> {
  final GlobalKey<ScaffoldState> _scaffoldKey = GlobalKey<ScaffoldState>();
  final MapController _map = MapController();
  TileSource _base = baseLayers.first;
  bool _followedFirstFix = false;

  // Chart orientation. north-up = rotation locked to 0; course-up = the chart
  // rotates so the boat's course-over-ground points to the top of the screen.
  bool _courseUp = false;
  double _rotationDeg = 0; // live map rotation, mirrored from the camera

  late final WindOverlayController _wind =
      WindOverlayController(state: widget.state);

  // Touch devices pinch-to-zoom, so the on-screen +/- buttons are redundant
  // there; keep them for mouse/trackpad (desktop, web).
  bool get _isTouch =>
      !kIsWeb &&
      (defaultTargetPlatform == TargetPlatform.iOS ||
          defaultTargetPlatform == TargetPlatform.android);

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

  Future<void> _toggleWind() async {
    // Seed the viewport so arrows show immediately on first load.
    _wind.bounds = _map.camera.visibleBounds;
    _wind.rotationDeg = _rotationDeg;
    setState(() {}); // reflect the spinner
    try {
      await _wind.toggle();
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text('Wind unavailable: $e')));
    }
    if (mounted) setState(() {});
  }

  /// Fetch the wind field at forecast hour [fh] and show it.
  Future<void> _loadWind(int fh) async {
    _wind.bounds = _map.camera.visibleBounds;
    _wind.rotationDeg = _rotationDeg;
    setState(() => _wind.loading = true);
    try {
      await _wind.load(fh);
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text('Wind unavailable: $e')));
    }
    if (mounted) setState(() {});
  }

  void _zoom(double delta) {
    final c = _map.camera;
    _map.move(c.center, (c.zoom + delta).clamp(3.0, 20.0));
  }

  void _toggleOrientation() {
    setState(() => _courseUp = !_courseUp);
    if (_courseUp) {
      _applyCourseUp();
    } else {
      _map.rotate(0); // snap back to north-up
    }
  }

  /// In course-up mode, rotate the chart so the boat's course points up.
  /// flutter_map's heading-up convention is rotate(-course).
  void _applyCourseUp() {
    final cog = widget.state.cogDeg;
    if (_courseUp && cog != null) _map.rotate(-cog);
  }

  void _onState() {
    // Recenter once when the first GPS fix arrives, then leave the user in
    // control of the viewport. (Task J4 replaces this with continuous follow
    // plus pan-to-suspend.)
    final pos = widget.state.position;
    if (!_followedFirstFix && pos != null) {
      _followedFirstFix = true;
      _map.move(pos, 13);
    }
    _applyCourseUp(); // keep the chart aligned as the course changes
    if (mounted) setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    final s = widget.state;
    return Scaffold(
      key: _scaffoldKey,
      endDrawer: DataDrawer(state: s, history: widget.connection.history),
      body: Stack(
        children: [
          FlutterMap(
            mapController: _map,
            options: MapOptions(
              initialCenter: const LatLng(41.3, -72.0), // Long Island Sound-ish
              initialZoom: 9,
              // No free rotation: chart orientation is only ever north-up or
              // course-up via the toggle, so the two-finger rotate gesture is
              // disabled (an accidental rotate at the helm is disorienting
              // and there'd be no way to know your bearing).
              interactionOptions: const InteractionOptions(
                flags: InteractiveFlag.all & ~InteractiveFlag.rotate,
              ),
              onPositionChanged: (camera, _) {
                _wind.bounds = camera.visibleBounds;
                _rotationDeg = camera.rotation;
                _wind.rotationDeg = camera.rotation;
                if (_wind.on && mounted) {
                  _wind.rebuildMarkers();
                  setState(() {});
                }
              },
            ),
            children: buildMapLayers(
              state: s,
              base: _base,
              rotationDeg: _rotationDeg,
              windOn: _wind.on,
              windMarkers: _wind.markers,
              onAisTap: (b) => showAisDetails(context, b),
            ),
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
                  child: StatusChip(state: s),
                ),
              ),
            ),
          ),
          // Top-center: glanceable next-waypoint distance + ETA while navigating.
          if (s.navigating)
            SafeArea(
              child: Align(
                alignment: Alignment.topCenter,
                child: Padding(
                  padding: const EdgeInsets.only(top: 12),
                  child: EtaPill(state: s),
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
                    LayerSwitcher(
                      current: _base,
                      onChanged: (t) => setState(() => _base = t),
                    ),
                    const SizedBox(height: 8),
                    MapRoundButton(
                      icon: Icons.dashboard,
                      tooltip: 'Boat data',
                      onTap: () => _scaffoldKey.currentState?.openEndDrawer(),
                    ),
                    const SizedBox(height: 8),
                    MapRoundButton(
                      icon: _courseUp ? Icons.navigation : Icons.explore,
                      tooltip: _courseUp ? 'Course up' : 'North up',
                      active: _courseUp,
                      onTap: _toggleOrientation,
                    ),
                    const SizedBox(height: 8),
                    MapRoundButton(
                      icon: Icons.air,
                      tooltip: 'Wind',
                      active: _wind.on,
                      busy: _wind.loading,
                      onTap: _toggleWind,
                    ),
                    if (s.cameraNames.isNotEmpty &&
                        widget.connection.robot != null) ...[
                      const SizedBox(height: 8),
                      MapRoundButton(
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
                    if (s.tanks.isNotEmpty) ...[
                      const SizedBox(height: 8),
                      MapRoundButton(
                        icon: Icons.local_gas_station,
                        tooltip: 'Fuel',
                        onTap: () => Navigator.of(context).push(
                          MaterialPageRoute<void>(
                            builder: (_) => FuelScreen(
                              state: widget.state,
                              history: widget.connection.history,
                            ),
                          ),
                        ),
                      ),
                    ],
                    if (widget.onSwitchBoat != null) ...[
                      const SizedBox(height: 8),
                      MapRoundButton(
                        icon: Icons.sailing,
                        tooltip: 'Switch boat',
                        onTap: widget.onSwitchBoat!,
                      ),
                    ],
                  ],
                ),
              ),
            ),
          ),
          // Center-right: zoom controls. Hidden on touch devices (pinch-zoom).
          if (!_isTouch)
            SafeArea(
              child: Align(
                alignment: Alignment.centerRight,
                child: Padding(
                  padding: const EdgeInsets.all(12),
                  child: Column(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      MapRoundButton(
                          icon: Icons.add,
                          tooltip: 'Zoom in',
                          onTap: () => _zoom(1)),
                      const SizedBox(height: 8),
                      MapRoundButton(
                          icon: Icons.remove,
                          tooltip: 'Zoom out',
                          onTap: () => _zoom(-1)),
                    ],
                  ),
                ),
              ),
            ),
          // Bottom: wind forecast-time slider (only while wind is on).
          if (_wind.on)
            Positioned(
              left: 12,
              right: 76, // clear the center-on-boat FAB
              bottom: 12,
              child: SafeArea(
                child: WindForecastBar(
                  fh: _wind.fh,
                  loading: _wind.loading,
                  onScrub: (v) => setState(() => _wind.fh = v),
                  onCommit: _loadWind,
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
