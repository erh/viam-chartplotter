import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/foundation.dart'
    show defaultTargetPlatform, kIsWeb, TargetPlatform;
import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import 'ais.dart';
import 'app_config.dart';
import 'map/ais_markers.dart';

import 'boat_state.dart';
import 'camera_screen.dart';
import 'data_drawer.dart';
import 'debug_screen.dart';
import 'fuel_screen.dart';
import 'map/ais_sheet.dart';
import 'map/map_controls.dart';
import 'map/map_layers.dart';
import 'map/wind_overlay.dart';
import 'settings.dart';
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
  final Settings _settings = Settings.instance;
  late TileSource _base = baseLayers.firstWhere(
      (b) => b.id == _settings.baseLayerId,
      orElse: () => baseLayers.first);
  bool _followedFirstFix = false;

  // A persisted view means the user deliberately left the map somewhere —
  // the first GPS fix must not stomp it (J6).
  late final bool _restoredView =
      _settings.mapCenter != null && _settings.mapZoom != null;
  Timer? _persistViewDebounce;

  // Chart orientation. north-up = rotation locked to 0; course-up = the chart
  // rotates so the boat's course-over-ground points to the top of the screen.
  late bool _courseUp = _settings.courseUp;
  double _rotationDeg = 0; // live map rotation, mirrored from the camera

  late final WindOverlayController _wind =
      WindOverlayController(state: widget.state);

  // Cached AIS marker layer (D10): rebuilt only when the AIS set or the
  // camera changes, never on the bare 1 Hz state tick, and bounded by a
  // viewport cull + cap so a busy harbour doesn't rebuild hundreds of
  // rotated widgets per second.
  List<Marker> _aisMarkers = const [];
  List<AisBoat>? _aisMarkersFor; // the list identity the cache was built from
  static const int _aisCap = 500;
  static const double _aisMinZoom = 7; // chart gate; heading invisible below

  void _rebuildAisMarkers() {
    final s = widget.state;
    final MapCamera camera;
    try {
      camera = _map.camera;
    } catch (_) {
      return; // map not built yet — the first onPositionChanged re-runs this
    }
    if (camera.zoom < _aisMinZoom) {
      widget.state.aisCulled = s.aisBoats.length;
      widget.state.aisCapped = 0;
      _aisMarkers = const [];
      _aisMarkersFor = s.aisBoats;
      return;
    }
    final result = cullAisTargets(
      boats: s.aisBoats,
      bounds: camera.visibleBounds,
      reference: camera.center,
      cap: _aisCap,
    );
    widget.state.aisCulled = result.culled;
    widget.state.aisCapped = result.capped;
    if (result.capped > 0) {
      debugPrint('AIS cap: drawing ${result.shown.length}, '
          'dropped ${result.capped} beyond cap (+${result.culled} off-screen)');
    }
    _aisMarkers = [
      for (final b in result.shown)
        Marker(
          point: b.location,
          width: 30,
          height: 30,
          child: GestureDetector(
            onTap: () => showAisDetails(context, b),
            child: Transform.rotate(
              angle: (b.orientationDeg + _rotationDeg) * math.pi / 180.0,
              child: const Icon(Icons.navigation,
                  color: Colors.cyanAccent, size: 22),
            ),
          ),
        ),
    ];
    _aisMarkersFor = s.aisBoats;
  }

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
    AppConfig.tileBase.addListener(_onTileBaseChanged);
    // Wind was on last session: refetch it once the first frame is up
    // (mobile launches are expensive enough that this is worth persisting
    // even though the web app doesn't).
    if (_settings.windOn) {
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted && !_wind.on) _toggleWind();
      });
    }
  }

  @override
  void dispose() {
    _persistViewDebounce?.cancel();
    AppConfig.tileBase.removeListener(_onTileBaseChanged);
    widget.state.removeListener(_onState);
    super.dispose();
  }

  /// /app-config moved the tile server (A6): re-resolve the selected base
  /// against the fresh layer list so tile traffic switches hosts live.
  void _onTileBaseChanged() {
    if (!mounted) return;
    setState(() {
      _base = baseLayers.firstWhere((b) => b.id == _base.id,
          orElse: () => baseLayers.first);
    });
  }

  /// Persist the camera after it settles — a debounce, not per-frame writes.
  void _persistView() {
    _persistViewDebounce?.cancel();
    _persistViewDebounce = Timer(const Duration(seconds: 1), () {
      final c = _map.camera;
      _settings.mapCenter = c.center;
      _settings.mapZoom = c.zoom;
    });
  }

  Future<void> _toggleWind() async {
    // Seed the viewport so arrows show immediately on first load.
    _wind.bounds = _map.camera.visibleBounds;
    _wind.rotationDeg = _rotationDeg;
    setState(() {}); // reflect the spinner
    _settings.windOn = !_wind.on;
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

  /// Chart settings dialog — today just the safe depth (A2), the single most
  /// safety-relevant chart parameter: solid coral below it, gradient to white
  /// at 2×. Empty = the server's configured default.
  Future<void> _editChartSettings() async {
    final controller = TextEditingController(
        text: _settings.safeDepthFt?.toString() ?? '');
    final apply = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Chart settings'),
        content: TextField(
          controller: controller,
          autofocus: true,
          keyboardType: TextInputType.number,
          decoration: const InputDecoration(
            labelText: 'Safe depth (ft)',
            helperText: 'Shades water shallower than your draft.\n'
                'Empty = server default.',
          ),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: const Text('Apply'),
          ),
        ],
      ),
    );
    if (apply != true || !mounted) return;
    final text = controller.text.trim();
    setState(() {
      // The tile layer's key includes this value, so the chart refetches
      // with the new shading immediately.
      _settings.safeDepthFt = text.isEmpty ? null : int.tryParse(text);
    });
  }

  void _zoom(double delta) {
    final c = _map.camera;
    _map.move(c.center, (c.zoom + delta).clamp(3.0, 20.0));
  }

  void _toggleOrientation() {
    setState(() => _courseUp = !_courseUp);
    _settings.courseUp = _courseUp;
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
    // control of the viewport — unless a persisted view was restored, which
    // the user deliberately left somewhere (J6). (Task J4 replaces this with
    // continuous follow plus pan-to-suspend.)
    final pos = widget.state.position;
    if (!_followedFirstFix && pos != null) {
      _followedFirstFix = true;
      if (!_restoredView) _map.move(pos, 13);
    }
    _applyCourseUp(); // keep the chart aligned as the course changes
    // The AIS marker cache keys off the list identity: the poll loop swaps
    // in a fresh list per AIS tick, so identical() is a change detector.
    if (!identical(widget.state.aisBoats, _aisMarkersFor)) {
      _rebuildAisMarkers();
    }
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
              // Resume the persisted view; Long Island Sound only on a truly
              // fresh install (J6).
              initialCenter:
                  _settings.mapCenter ?? const LatLng(41.3, -72.0),
              initialZoom: _settings.mapZoom ?? 9,
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
                _persistView();
                _rebuildAisMarkers(); // viewport moved → re-cull (D10)
                if (mounted) {
                  if (_wind.on) _wind.rebuildMarkers();
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
              aisMarkers: _aisMarkers,
              buildStamp: _settings.buildStamp,
              safeDepthFt: _settings.safeDepthFt,
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
                  child: StatusChip(
                    state: s,
                    onReconnect: widget.connection.reconnectNow,
                  ),
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
                      onChanged: (t) {
                        setState(() => _base = t);
                        _settings.baseLayerId = t.id;
                      },
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
                    const SizedBox(height: 8),
                    MapRoundButton(
                      icon: Icons.settings,
                      tooltip: 'Chart settings',
                      onTap: _editChartSettings,
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
