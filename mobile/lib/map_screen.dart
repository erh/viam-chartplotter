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
import 'map/heading_line.dart';
import 'map/measure.dart';
import 'map/nautical_scalebar.dart';

import 'boat_state.dart';
import 'camera_screen.dart';
import 'data_drawer.dart';
import 'debug_screen.dart';
import 'fuel_screen.dart';
import 'chart/navaid_icon.dart';
import 'chart/navaids.dart';
import 'chart/structure_icon.dart';
import 'chart/structures.dart';
import 'chart/areas.dart';
import 'chart/bbox_source.dart';
import 'map/ais_sheet.dart';
import 'map/map_controls.dart';
import 'map/map_layers.dart';
import 'map/tile_cache.dart';
import 'map/wind_overlay.dart';
import 'routes/route_store.dart' show NavWaypoint;
import 'routes/routes_sheet.dart';
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
    // Armed waypoint edits (E1) take the tap first: move, then insert. Both
    // commit exactly one RPC per gesture (tap-to-place, not drag-per-frame).
    final movingId = _movingWaypointId;
    if (movingId != null) {
      setState(() => _movingWaypointId = null);
      _navEdit(() => widget.connection.moveNavWaypoint(movingId, point));
      return;
    }
    final beforeId = _insertBeforeId;
    if (beforeId != null) {
      setState(() => _insertBeforeId = null);
      _navEdit(
          () => widget.connection.insertNavWaypointBefore(beforeId, point));
      return;
    }
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

  // ---- waypoint editing (E1) -------------------------------------------
  // Long-press adds; tapping a waypoint opens a sheet with move / insert /
  // delete / clear. Move and insert arm a tap-to-place mode (banner below):
  // one deliberate tap = one move_waypoint/insert_waypoint, which is the
  // touch equivalent of the web's drag-commit-on-release.
  String? _movingWaypointId;
  String? _insertBeforeId;

  bool get _waypointModeArmed =>
      _movingWaypointId != null || _insertBeforeId != null;

  /// Run one waypoint edit, surfacing failure — the optimistic state was
  /// already rolled back by the connection's refetch.
  Future<void> _navEdit(Future<void> Function() fn) async {
    try {
      await fn();
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text('Waypoint edit failed: $e')));
    }
  }

  void _onMapLongPress(LatLng point) {
    if (widget.connection.navApi == null) return;
    if (_measureMode || _waypointModeArmed) return; // don't fight other modes
    _navEdit(() => widget.connection.addNavWaypoint(point));
  }

  void _showWaypointSheet(int index, NavWaypoint w) {
    final total = widget.state.navWaypoints.length;
    showModalBottomSheet<void>(
      context: context,
      showDragHandle: true,
      builder: (ctx) => SafeArea(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            ListTile(
              title: Text('Waypoint ${index + 1} of $total'),
              subtitle: Text(
                  '${w.pos.latitude.toStringAsFixed(5)}, '
                  '${w.pos.longitude.toStringAsFixed(5)}'
                  '${w.isPending ? ' · syncing…' : ''}'),
            ),
            ListTile(
              leading: const Icon(Icons.open_with),
              title: const Text('Move — then tap the new spot'),
              enabled: !w.isPending,
              onTap: () {
                Navigator.pop(ctx);
                setState(() {
                  _movingWaypointId = w.id;
                  _insertBeforeId = null;
                });
              },
            ),
            ListTile(
              leading: const Icon(Icons.add_location_alt),
              title: const Text('Insert before — tap where'),
              enabled: !w.isPending,
              onTap: () {
                Navigator.pop(ctx);
                setState(() {
                  _insertBeforeId = w.id;
                  _movingWaypointId = null;
                });
              },
            ),
            ListTile(
              leading: const Icon(Icons.delete_outline),
              title: const Text('Delete waypoint'),
              onTap: () {
                Navigator.pop(ctx);
                _navEdit(() => widget.connection.removeNavWaypoint(w.id));
              },
            ),
            ListTile(
              leading: const Icon(Icons.clear_all, color: Colors.redAccent),
              title: const Text('Clear entire route…',
                  style: TextStyle(color: Colors.redAccent)),
              onTap: () async {
                Navigator.pop(ctx);
                // Deliberate two-step confirm (web arms clearConfirmArmed).
                final ok = await showDialog<bool>(
                  context: context,
                  builder: (dctx) => AlertDialog(
                    title: const Text('Clear route?'),
                    content: Text('Remove all $total waypoints?'),
                    actions: [
                      TextButton(
                          onPressed: () => Navigator.pop(dctx, false),
                          child: const Text('Cancel')),
                      FilledButton(
                        style: FilledButton.styleFrom(
                            backgroundColor: Colors.red),
                        onPressed: () => Navigator.pop(dctx, true),
                        child: const Text('Clear route'),
                      ),
                    ],
                  ),
                );
                if (ok == true) {
                  await _navEdit(widget.connection.clearNavWaypoints);
                }
              },
            ),
            const SizedBox(height: 8),
          ],
        ),
      ),
    );
  }

  /// Numbered, tappable waypoint markers; pending (unacknowledged) ones
  /// render translucent.
  List<Marker> _waypointMarkers() {
    final wps = widget.state.navWaypoints;
    return [
      for (var i = 0; i < wps.length; i++)
        Marker(
          point: wps[i].pos,
          width: 40,
          height: 40,
          child: GestureDetector(
            behavior: HitTestBehavior.opaque,
            onTap: () => _showWaypointSheet(i, wps[i]),
            child: Center(
              child: Container(
                width: 26,
                height: 26,
                alignment: Alignment.center,
                decoration: BoxDecoration(
                  shape: BoxShape.circle,
                  color: Colors.purpleAccent
                      .withValues(alpha: wps[i].isPending ? 0.45 : 0.95),
                  border: Border.all(color: Colors.white, width: 2),
                ),
                child: Text(
                  '${i + 1}',
                  style: const TextStyle(
                      color: Colors.white,
                      fontSize: 12,
                      fontWeight: FontWeight.bold),
                ),
              ),
            ),
          ),
        ),
    ];
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

  // Saved-route previews (E2): pushed up by the routes sheet, kept on the
  // chart after it closes (the sheet covers the map, so previews are only
  // visible once it's gone). The toggle state rides along so a reopened
  // sheet shows what's previewed.
  Map<String, RoutePreview> _routePreviews = const {};
  Set<String> _previewedRouteIds = const {};
  bool _previewShowAll = false;
  // Save-from-track candidate (E3): the simplified geometry being previewed
  // in the sheet's dialog — exactly what would be saved.
  List<LatLng>? _trackCandidate;

  void _openRoutesSheet() {
    final api = widget.connection.navApi;
    if (api == null) return;
    showModalBottomSheet<void>(
      context: context,
      showDragHandle: true,
      isScrollControlled: true,
      builder: (_) => RoutesSheet(
        state: widget.state,
        api: api,
        onLoad: widget.connection.loadRouteWaypoints,
        previewedIds: _previewedRouteIds,
        showAll: _previewShowAll,
        fetchTrack: (widget.connection.history?.hasTrackWindow ?? false)
            ? widget.connection.history!.fetchTrackWindow
            : null,
        onTrackPreview: (points) {
          if (mounted) setState(() => _trackCandidate = points);
        },
        onPreviews: (previews, ids, showAll) {
          if (!mounted) return;
          setState(() {
            _routePreviews = previews;
            _previewedRouteIds = ids;
            _previewShowAll = showAll;
          });
        },
      ),
    );
  }

  // Per-area visibility (B3), seeded from the season window on every load
  // and deliberately NOT persisted (web does the same): a stale preference
  // would leave a seasonal closure switched on long after its window.
  final Map<String, bool> _areaOn = {};

  bool _areaVisible(AreaInfo a) => _areaOn[a.name] ?? a.inSeason;

  List<AreaInfo> get _visibleAreas =>
      [for (final a in widget.state.areas) if (_areaVisible(a)) a];

  // Navaids vector layer (B1): viewport-loaded via B4, shown at z >= 12
  // (below that the tile bakes nothing to replace — the tier is off — and
  // symbols would be soup anyway).
  static const double navaidMinZoom = 12;
  late final BboxFeatureSource<NavaidFeature> _navaids =
      BboxFeatureSource<NavaidFeature>(
    fetch: (w, s, e, n) =>
        fetchNavaids(AppConfig.tileBase.value, w, s, e, n),
  )..addListener(_rebuildNavaidsAndRepaint);
  List<Marker> _navaidMarkers = const [];

  void _rebuildNavaidsAndRepaint() {
    _rebuildNavaidMarkers();
    if (mounted) setState(() {});
  }

  void _rebuildNavaidMarkers() {
    final MapCamera camera;
    try {
      camera = _map.camera;
    } catch (_) {
      return;
    }
    if (camera.zoom < navaidMinZoom) {
      _navaidMarkers = const [];
      return;
    }
    final b = camera.visibleBounds;
    final latM = (b.north - b.south) * 0.2;
    final lonM = (b.east - b.west) * 0.2;
    _navaidMarkers = [
      for (final f in _navaids.features)
        if (f.pos.latitude >= b.south - latM &&
            f.pos.latitude <= b.north + latM &&
            f.pos.longitude >= b.west - lonM &&
            f.pos.longitude <= b.east + lonM)
          Marker(
            point: f.pos,
            // 44 pt tap target around the 24 px symbol; the footprint pixel
            // (12,18 of 24) sits at ~64% down the box — alignment keeps the
            // chart fix under the structure, letting the flare float above.
            width: 44,
            height: 44,
            alignment: const Alignment(0, 0.27),
            child: GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: () => _showNavaidSheet(f),
              child: Center(child: NavaidIcon(feature: f)),
            ),
          ),
    ];
  }

  void _showNavaidSheet(NavaidFeature f) {
    final lines = navaidSheetLines(f.props);
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
              Row(children: [
                NavaidIcon(feature: f),
                const SizedBox(width: 10),
                Expanded(
                  child: Text(lines.first,
                      style: Theme.of(ctx).textTheme.titleLarge),
                ),
              ]),
              const SizedBox(height: 4),
              for (final l in lines.skip(1))
                Padding(
                  padding: const EdgeInsets.symmetric(vertical: 3),
                  child: Text(l, style: const TextStyle(fontSize: 15)),
                ),
            ],
          ),
        ),
      ),
    );
  }

  // Structures vector layer (B2): bridges / overhead cables / pipes /
  // conveyors, viewport-loaded, shown at z >= 14 where the tile skips them.
  static const double structureMinZoom = 14;
  late final BboxFeatureSource<StructureFeature> _structures =
      BboxFeatureSource<StructureFeature>(
    fetch: (w, s, e, n) =>
        fetchStructures(AppConfig.tileBase.value, w, s, e, n),
  )..addListener(_rebuildStructuresAndRepaint);
  List<Marker> _structureMarkers = const [];
  List<StructureFeature> _structuresInView = const [];

  void _rebuildStructuresAndRepaint() {
    _rebuildStructureMarkers();
    if (mounted) setState(() {});
  }

  void _rebuildStructureMarkers() {
    final MapCamera camera;
    try {
      camera = _map.camera;
    } catch (_) {
      return;
    }
    if (camera.zoom < structureMinZoom) {
      _structureMarkers = const [];
      _structuresInView = const [];
      return;
    }
    final b = camera.visibleBounds;
    final latM = (b.north - b.south) * 0.2;
    final lonM = (b.east - b.west) * 0.2;
    _structuresInView = [
      for (final f in _structures.features)
        if (f.anchor.latitude >= b.south - latM &&
            f.anchor.latitude <= b.north + latM &&
            f.anchor.longitude >= b.west - lonM &&
            f.anchor.longitude <= b.east + lonM)
          f,
    ];
    _structureMarkers = [
      for (final f in _structuresInView)
        if (!f.hideIcon)
          Marker(
            point: f.anchor,
            width: 44,
            height: 44,
            child: GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: () => _showStructureSheet(f),
              child: Center(child: StructureIcon(class_: f.class_)),
            ),
          ),
    ];
  }

  void _showStructureSheet(StructureFeature f) {
    final lines = structureSheetLines(f.props);
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
              Row(children: [
                StructureIcon(class_: f.class_),
                const SizedBox(width: 10),
                Expanded(
                  child: Text(lines.first,
                      style: Theme.of(ctx).textTheme.titleLarge),
                ),
              ]),
              const SizedBox(height: 4),
              for (final l in lines.skip(1))
                Padding(
                  padding: const EdgeInsets.symmetric(vertical: 3),
                  child: Text(l, style: const TextStyle(fontSize: 15)),
                ),
            ],
          ),
        ),
      ),
    );
  }

  // Cached AIS marker layer (D10): rebuilt only when the AIS set or the
  // camera changes, never on the bare 1 Hz state tick, and bounded by a
  // viewport cull + cap so a busy harbour doesn't rebuild hundreds of
  // rotated widgets per second.
  List<Marker> _aisMarkers = const [];
  List<List<LatLng>> _aisProjections = const []; // COG vectors (D2)
  List<List<LatLng>> _aisTracks = const []; // history tracks (D1)
  List<AisBoat>? _aisMarkersFor; // the list identity the cache was built from
  Map<String, List<LatLng>>? _aisTracksForHistory; // history-map identity
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
      _aisProjections = const [];
      _aisTracks = const [];
      _aisMarkersFor = s.aisBoats;
      _aisTracksForHistory = s.aisHistory;
      return;
    }
    final result = cullAisTargets(
      boats: s.aisBoats,
      bounds: camera.visibleBounds,
      reference: camera.center,
      // Threatening targets (real upcoming CPA) survive the cap first (D3).
      own: s.position != null
          ? (
              lat: s.position!.latitude,
              lng: s.position!.longitude,
              cogDeg: s.cogDeg,
              spdKn: s.speedKn ?? 0,
            )
          : null,
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
            onTap: () {
              unawaited(widget.connection.fetchAisTrack(b.mmsi));
              showAisDetails(context, b, own: widget.state);
            },
            child: Transform.rotate(
              angle: (b.orientationDeg + _rotationDeg) * math.pi / 180.0,
              child: const Icon(Icons.navigation,
                  color: Colors.cyanAccent, size: 22),
            ),
          ),
        ),
    ];
    // Projection vectors (D2): where each shown target will be in N minutes
    // along its COG at its SOG.
    final projMin = _settings.aisProjectionMin;
    _aisProjections = [
      for (final b in result.shown)
        if (projectionPoints(b.location, b.cogDeg, b.sogKn, projMin)
            case final p?)
          p,
    ];
    // History tracks (D1) for the shown targets only (shares D10's cull).
    _aisTracks = _settings.aisTracksOn
        ? [
            for (final b in result.shown)
              if (s.aisHistory[b.mmsi] case final pts?
                  when pts.length >= 2)
                pts,
          ]
        : const [];
    _aisMarkersFor = s.aisBoats;
    _aisTracksForHistory = s.aisHistory;
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
    _navaids.dispose();
    _structures.dispose();
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

  /// Keep the boat anchored while following (J4). Follows the displayed
  /// fix — the phone's when the boat's has gone stale (L5).
  void _followTick() {
    if (!_followBoat) return;
    final pos = widget.state.displayPosition;
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
    final tileCache = TileDiskCache.instance;
    var cacheBytes = await tileCache?.usageBytes();
    if (!mounted) return;
    var depthColor = _settings.depthColorTrack;
    var headingOn = _settings.headingLineOn;
    var headingLen = _settings.headingLineLengthNm;
    var boatBottom = _settings.boatPositionBottom;
    var projMin = _settings.aisProjectionMin;
    var aisTracks = _settings.aisTracksOn;
    var webSenders = _settings.webSendersOn;
    final areaToggles = Map<String, bool>.of(_areaOn);
    final apply = await showDialog<bool>(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialogState) => AlertDialog(
        title: const Text('Chart settings'),
        // The settings list outgrew the dialog box: scroll it, size it to
        // the screen, and don't autofocus the depth field — the keyboard
        // eating half the height is what made it unusable on the phone.
        content: SizedBox(
          width: double.maxFinite,
          child: SingleChildScrollView(
            child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            TextField(
              controller: controller,
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
              title: const Text('Web sender targets'),
              subtitle: const Text('Positions rebroadcast via the web'),
              value: webSenders,
              onChanged: (v) => setDialogState(() => webSenders = v),
            ),
            SwitchListTile(
              contentPadding: EdgeInsets.zero,
              title: const Text('AIS tracks'),
              subtitle: const Text('History trails behind targets'),
              value: aisTracks,
              onChanged: (v) => setDialogState(() => aisTracks = v),
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
            if (widget.state.areas.isNotEmpty) ...[
              const Padding(
                padding: EdgeInsets.only(top: 12, bottom: 4),
                child: Align(
                  alignment: Alignment.centerLeft,
                  child: Text('Areas',
                      style: TextStyle(fontWeight: FontWeight.bold)),
                ),
              ),
              for (final a in widget.state.areas)
                SwitchListTile(
                  contentPadding: EdgeInsets.zero,
                  dense: true,
                  title: Text(
                      '${a.folder != null ? '${a.folder} / ' : ''}${a.name}'),
                  subtitle: areaDateSuffix(a.startDate, a.endDate).isEmpty
                      ? null
                      : Text(areaDateSuffix(a.startDate, a.endDate).trim()),
                  value: areaToggles[a.name] ?? a.inSeason,
                  onChanged: (v) =>
                      setDialogState(() => areaToggles[a.name] = v),
                ),
            ],
            Row(
              children: [
                const Expanded(child: Text('Projection vectors')),
                DropdownButton<int>(
                  value: projMin,
                  items: [
                    for (final n in aisProjectionChoices)
                      DropdownMenuItem(value: n, child: Text('$n min')),
                  ],
                  onChanged: (v) =>
                      setDialogState(() => projMin = v ?? projMin),
                ),
              ],
            ),
            // Tile disk cache (L1): usage + clear.
            if (tileCache != null)
              Row(
                children: [
                  Expanded(
                    child: Text('Tile cache: '
                        '${((cacheBytes ?? 0) / (1024 * 1024)).toStringAsFixed(0)}'
                        ' MB of '
                        '${(tileCache.maxBytes / (1024 * 1024)).round()} MB'),
                  ),
                  TextButton(
                    onPressed: () async {
                      await tileCache.clear();
                      cacheBytes = await tileCache.usageBytes();
                      setDialogState(() {});
                    },
                    child: const Text('Clear'),
                  ),
                ],
              ),
          ],
            ),
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
      _settings.aisProjectionMin = projMin;
      _settings.aisTracksOn = aisTracks;
      _settings.webSendersOn = webSenders;
      _areaOn
        ..clear()
        ..addAll(areaToggles);
    });
    _rebuildAisMarkers(); // projection minutes may have changed
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
    if (!identical(widget.state.aisBoats, _aisMarkersFor) ||
        !identical(widget.state.aisHistory, _aisTracksForHistory)) {
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
              // Long-press adds a waypoint (E1) — deliberately a different
              // gesture from the measure tool's taps so they can't fight.
              onLongPress: (_, point) => _onMapLongPress(point),
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
                if (camera.zoom >= navaidMinZoom) {
                  _navaids.viewportChanged(camera.visibleBounds);
                }
                _rebuildNavaidMarkers();
                if (camera.zoom >= structureMinZoom) {
                  _structures.viewportChanged(camera.visibleBounds);
                }
                _rebuildStructureMarkers();
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
              areas: _visibleAreas,
              navaidMarkers: _navaidMarkers,
              structureMarkers: _structureMarkers,
              structures: _structuresInView,
              aisProjections: _aisProjections,
              aisTracks: _aisTracks,
              routePreviews: [
                ..._routePreviews.values,
                if (_trackCandidate case final tc? when tc.length >= 2)
                  (points: tc, color: Colors.tealAccent),
              ],
              // Active nav route (E1): boat (fresh fix only) + the chain.
              activeRoutePoints: s.navWaypoints.isEmpty
                  ? const []
                  : [
                      if (s.boatFixFresh && s.position != null) s.position!,
                      for (final w in s.navWaypoints) w.pos,
                    ],
              waypointMarkers: _waypointMarkers(),
              ownProjection: (s.boatFixFresh && s.position != null)
                  ? (projectionPoints(s.position!, s.cogDeg, s.speedKn ?? 0,
                          _settings.aisProjectionMin) ??
                      const [])
                  : const [],
              trackSegments: _trackSegments(),
              // Heading line only off a live BOAT fix — a stale heading off
              // the phone's position would be a lie (L5). Uses the effective
              // heading (COG under way), like the web app.
              headingLine: (_settings.headingLineOn &&
                      s.boatFixFresh &&
                      s.position != null &&
                      s.effectiveHeadingDeg != null)
                  ? headingLinePoints(s.position!, s.effectiveHeadingDeg!,
                      _settings.headingLineLengthNm.toDouble())
                  : const [],
              headingTicks: (_settings.headingLineOn &&
                      s.boatFixFresh &&
                      s.position != null &&
                      s.effectiveHeadingDeg != null)
                  ? headingLineTicks(s.position!, s.effectiveHeadingDeg!,
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
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      StatusChip(
                        state: s,
                        onReconnect: widget.connection.reconnectNow,
                      ),
                      // Never a silent substitute (L5): when the marker is
                      // the phone's fix, say so where the status lives.
                      if (s.usingPhoneGps)
                        Container(
                          margin: const EdgeInsets.only(top: 6),
                          padding: const EdgeInsets.symmetric(
                              horizontal: 10, vertical: 4),
                          decoration: BoxDecoration(
                            color: Colors.amber,
                            borderRadius: BorderRadius.circular(12),
                          ),
                          child: const Text(
                            'PHONE GPS',
                            style: TextStyle(
                                color: Colors.black,
                                fontSize: 11,
                                fontWeight: FontWeight.bold),
                          ),
                        ),
                    ],
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
          // Waypoint move/insert armed (E1): say what the next tap does.
          if (_waypointModeArmed)
            SafeArea(
              child: Align(
                alignment: Alignment.topCenter,
                child: Padding(
                  padding: const EdgeInsets.only(top: 56),
                  child: Container(
                    padding: const EdgeInsets.only(left: 14),
                    decoration: BoxDecoration(
                      color: Colors.black.withValues(alpha: 0.7),
                      borderRadius: BorderRadius.circular(20),
                      border: Border.all(color: Colors.purpleAccent),
                    ),
                    child: Row(
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        Text(
                          _movingWaypointId != null
                              ? 'Tap the waypoint’s new spot'
                              : 'Tap where to insert the waypoint',
                          style: const TextStyle(
                              color: Colors.purpleAccent,
                              fontWeight: FontWeight.bold),
                        ),
                        TextButton(
                          onPressed: () => setState(() {
                            _movingWaypointId = null;
                            _insertBeforeId = null;
                          }),
                          child: const Text('Cancel'),
                        ),
                      ],
                    ),
                  ),
                ),
              ),
            ),
          // Top-center: glanceable next-waypoint distance + ETA while
          // navigating — via the route sensor or an active nav route (E5).
          if (s.navigating || s.navWaypoints.isNotEmpty)
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
                    if (widget.connection.navApi != null) ...[
                      const SizedBox(height: 8),
                      MapRoundButton(
                        icon: Icons.route,
                        tooltip: 'Routes',
                        onTap: _openRoutesSheet,
                      ),
                    ],
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
      floatingActionButton: (s.displayPosition == null || _followBoat)
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
