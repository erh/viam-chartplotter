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
import 'map/boat_icon.dart';
import 'map/heading_line.dart';
import 'map/measure.dart';
import 'map/nautical_scalebar.dart';

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
import 'track.dart';
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
  // the first GPS fix must not stomp it (J6), and follow starts suspended.
  late final bool _restoredView =
      _settings.mapCenter != null && _settings.mapZoom != null;
  Timer? _persistViewDebounce;

  // Follow mode (J4): keep the boat anchored on screen on every position
  // update; a user drag suspends it, the FAB resumes.
  late bool _followBoat = !_restoredView;

  // Measure tool (J1): tap sets the anchor, a second tap the end, further
  // taps restart from a new anchor; toggling the tool off clears.
  bool _measureMode = false;
  LatLng? _measureA;
  LatLng? _measureB;

  void _toggleMeasure() {
    setState(() {
      _measureMode = !_measureMode;
      _measureA = null;
      _measureB = null;
    });
  }

  void _onMapTap(LatLng point) {
    if (!_measureMode) return;
    setState(() {
      if (_measureA == null || _measureB != null) {
        _measureA = point; // first tap, or restart after a complete leg
        _measureB = null;
      } else {
        _measureB = point;
      }
    });
  }

  // Chart orientation. north-up = rotation locked to 0; course-up = the chart
  // rotates so the boat's course-over-ground points to the top of the screen.
  late bool _courseUp = _settings.courseUp;
  double _rotationDeg = 0; // live map rotation, mirrored from the camera

  late final WindOverlayController _wind =
      WindOverlayController(state: widget.state);

  // Cached track segments (C2): recomputed only when the track grew or the
  // depth-colour mode flipped, not on every rebuild.
  List<({List<LatLng> points, Color color})> _trackSegs = const [];
  int _trackSegsForLen = -1;
  bool _trackSegsForDepthMode = false;

  List<({List<LatLng> points, Color color})> _trackSegments() {
    final pts = widget.state.track.points;
    final depthMode = _settings.depthColorTrack;
    if (pts.length != _trackSegsForLen ||
        depthMode != _trackSegsForDepthMode) {
      _trackSegs = trackSegments(pts, colorByDepth: depthMode);
      _trackSegsForLen = pts.length;
      _trackSegsForDepthMode = depthMode;
    }
    return _trackSegs;
  }

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
    // Operator's own-boat icon, probed once per session (C1). Repaints on
    // the next state tick if an override loads.
    unawaited(MyBoatIcon.probe());
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
  /// While following, only the zoom is saved: a saved center means "the user
  /// deliberately left the map somewhere" (it also decides whether the next
  /// launch starts suspended), and web clears it on re-anchor the same way.
  void _persistView() {
    _persistViewDebounce?.cancel();
    _persistViewDebounce = Timer(const Duration(seconds: 1), () {
      final c = _map.camera;
      if (!_followBoat) _settings.mapCenter = c.center;
      _settings.mapZoom = c.zoom;
    });
  }

  /// Never trust a null-island fix (web guards this too, isValidCoordinate):
  /// a boat with no GPS reports [0,0] and would yank the chart to the Gulf
  /// of Guinea.
  static bool _validPos(LatLng p) =>
      !(p.latitude == 0 && p.longitude == 0) &&
      p.latitude.abs() <= 90 &&
      p.longitude.abs() <= 180;

  /// Screen offset for the followed boat (J2): centred, or 80% down for
  /// look-ahead. Rotation-aware — flutter_map applies the offset in screen
  /// space, so course-up keeps the look-ahead ahead.
  Offset _followOffset() {
    if (!_settings.boatPositionBottom) return Offset.zero;
    final h = MediaQuery.maybeSizeOf(context)?.height ?? 0;
    return Offset(0, h * 0.3); // center (50%) + 30% = 80% down
  }

  /// Keep the boat anchored while following (J4).
  void _followTick() {
    if (!_followBoat) return;
    final pos = widget.state.position;
    if (pos == null || !_validPos(pos)) return;
    try {
      _map.move(pos, _map.camera.zoom, offset: _followOffset());
    } catch (_) {
      // Map not built yet — the next tick follows.
    }
  }

  /// Resume following (the FAB): clears the persisted center, matching the
  /// web's re-anchor behaviour.
  void _resumeFollow() {
    setState(() => _followBoat = true);
    _settings.mapCenter = null;
    _followTick();
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

  /// Chart settings dialog: the safe depth (A2 — the single most
  /// safety-relevant chart parameter: solid coral below it, gradient to
  /// white at 2×; empty = the server's configured default) and the track's
  /// depth-colour mode (C2).
  Future<void> _editChartSettings() async {
    final controller = TextEditingController(
        text: _settings.safeDepthFt?.toString() ?? '');
    var depthColor = _settings.depthColorTrack;
    var headingOn = _settings.headingLineOn;
    var headingLen = _settings.headingLineLengthNm;
    var boatBottom = _settings.boatPositionBottom;
    final apply = await showDialog<bool>(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialogState) => AlertDialog(
        title: const Text('Chart settings'),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            TextField(
              controller: controller,
              autofocus: true,
              keyboardType: TextInputType.number,
              decoration: const InputDecoration(
                labelText: 'Safe depth (ft)',
                helperText: 'Shades water shallower than your draft.\n'
                    'Empty = server default.',
              ),
            ),
            const SizedBox(height: 8),
            SwitchListTile(
              contentPadding: EdgeInsets.zero,
              title: const Text('Colour track by depth'),
              subtitle: const Text('Shoal water reads red, 10 ft+ green'),
              value: depthColor,
              onChanged: (v) => setDialogState(() => depthColor = v),
            ),
            SwitchListTile(
              contentPadding: EdgeInsets.zero,
              title: const Text('Heading line'),
              value: headingOn,
              onChanged: (v) => setDialogState(() => headingOn = v),
            ),
            SwitchListTile(
              contentPadding: EdgeInsets.zero,
              title: const Text('Boat low on screen'),
              subtitle: const Text('Look-ahead: boat sits 80% down'),
              value: boatBottom,
              onChanged: (v) => setDialogState(() => boatBottom = v),
            ),
            Row(
              children: [
                const Expanded(child: Text('Heading line length')),
                DropdownButton<int>(
                  value: headingLen,
                  items: [
                    for (final n in headingLineLengthChoices)
                      DropdownMenuItem(value: n, child: Text('$n nm')),
                  ],
                  onChanged: (v) =>
                      setDialogState(() => headingLen = v ?? headingLen),
                ),
              ],
            ),
          ],
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
      ),
    );
    if (apply != true || !mounted) return;
    final text = controller.text.trim();
    setState(() {
      // The tile layer's key includes this value, so the chart refetches
      // with the new shading immediately.
      _settings.safeDepthFt = text.isEmpty ? null : int.tryParse(text);
      _settings.depthColorTrack = depthColor;
      _settings.headingLineOn = headingOn;
      _settings.headingLineLengthNm = headingLen;
      _settings.boatPositionBottom = boatBottom;
    });
    _followTick(); // re-anchor immediately if the screen position changed
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
    // First valid fix while following: jump to a useful zoom, then the
    // follow tick below keeps the boat anchored every update (J4).
    final pos = widget.state.position;
    if (!_followedFirstFix && pos != null && _validPos(pos)) {
      _followedFirstFix = true;
      if (_followBoat && !_restoredView) {
        try {
          _map.move(pos, 13);
        } catch (_) {}
      }
    }
    _applyCourseUp(); // keep the chart aligned as the course changes
    _followTick();
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
              onTap: (_, point) => _onMapTap(point),
              // Suspend follow on a user DRAG only (J4) — not pinch-zoom,
              // which should keep the boat anchored (J2), and not
              // programmatic moves. The web app documents why a
              // center-diff check false-positives here (:1193).
              onMapEvent: (evt) {
                if (!_followBoat) return;
                if (evt.source == MapEventSource.onDrag ||
                    evt.source == MapEventSource.dragStart ||
                    evt.source == MapEventSource.flingAnimationController) {
                  setState(() => _followBoat = false);
                }
              },
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
            children: [
              ...buildMapLayers(
              state: s,
              base: _base,
              rotationDeg: _rotationDeg,
              windOn: _wind.on,
              windMarkers: _wind.markers,
              aisMarkers: _aisMarkers,
              trackSegments: _trackSegments(),
              headingLine: (_settings.headingLineOn &&
                      s.position != null &&
                      s.headingDeg != null)
                  ? headingLinePoints(s.position!, s.headingDeg!,
                      _settings.headingLineLengthNm.toDouble())
                  : const [],
              buildStamp: _settings.buildStamp,
              // Fractional VIEW zoom for the OSM suppression gate (A5) —
              // deliberately not the tile z; see OsmUnderlayTileProvider.
              viewZoom: () {
                try {
                  return _map.camera.zoom;
                } catch (_) {
                  return 0;
                }
              },
              safeDepthFt: _settings.safeDepthFt,
              ),
              // Measure tool overlay (J1): anchor/end pins and the leg.
              if (_measureA != null && _measureB != null)
                PolylineLayer(
                  polylines: [
                    Polyline(
                      points: [_measureA!, _measureB!],
                      color: Colors.yellowAccent,
                      strokeWidth: 2,
                    ),
                  ],
                ),
              if (_measureMode)
                MarkerLayer(
                  markers: [
                    for (final p in [_measureA, _measureB])
                      if (p != null)
                        Marker(
                          point: p,
                          width: 18,
                          height: 18,
                          child: const Icon(Icons.circle,
                              size: 12, color: Colors.yellowAccent),
                        ),
                  ],
                ),
              // Scale bar in nm (J7), lifted clear of the wind slider.
              NauticalScalebar(liftPx: _wind.on ? 64 : 0),
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
                  child: StatusChip(
                    state: s,
                    onReconnect: widget.connection.reconnectNow,
                  ),
                ),
              ),
            ),
          ),
          // Measure readout (J1): top-center, clear of thumbs on the pins.
          if (_measureMode)
            SafeArea(
              child: Align(
                alignment: Alignment.topCenter,
                child: Padding(
                  padding: const EdgeInsets.only(top: 56),
                  child: Container(
                    padding: const EdgeInsets.symmetric(
                        horizontal: 14, vertical: 8),
                    decoration: BoxDecoration(
                      color: Colors.black.withValues(alpha: 0.7),
                      borderRadius: BorderRadius.circular(20),
                      border: Border.all(color: Colors.yellowAccent),
                    ),
                    child: Text(
                      _measureA == null
                          ? 'Measure: tap the start point'
                          : _measureB == null
                              ? 'Measure: tap the far end'
                              : measureLabel(_measureA!, _measureB!),
                      style: const TextStyle(
                          color: Colors.yellowAccent,
                          fontWeight: FontWeight.bold),
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
                      icon: Icons.straighten,
                      tooltip: 'Measure distance/bearing',
                      active: _measureMode,
                      onTap: _toggleMeasure,
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
      // The follow affordance (J4): hidden while following (the boat is
      // already anchored); after a drag suspends follow it appears as the
      // way back.
      floatingActionButton: (s.position == null || _followBoat)
          ? null
          : FloatingActionButton.extended(
              onPressed: _resumeFollow,
              tooltip: 'Resume following the boat',
              icon: const Icon(Icons.my_location),
              label: const Text('Follow'),
            ),
    );
  }
}
