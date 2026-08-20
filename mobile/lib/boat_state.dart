import 'package:flutter/foundation.dart';
import 'package:latlong2/latlong.dart';

import 'ais.dart';
import 'chart/areas.dart';
import 'routes/route_stats.dart';
import 'routes/route_store.dart';
import 'tides.dart';
import 'track.dart';

/// One tank's live reading plus the two freshness timestamps the fuel page
/// color-codes: when the app last successfully fetched the sensor, and when
/// the boat-side sensor last received data (viamboat's `_last_update`
/// reading; absent on module builds that predate it).
class TankStatus {
  const TankStatus({
    required this.name,
    this.level,
    this.capacity,
    this.fetchedAt,
    this.boatUpdatedAt,
    this.boatTimestampInvalid = false,
    this.levelIsFresh = true,
  });

  final String name;
  final double? level; // percent, last known (null = never read)
  final double? capacity; // liters, when the sensor reports one
  final DateTime? fetchedAt; // last successful app fetch from the boat
  final DateTime? boatUpdatedAt; // last CAN data arrival at the boat sensor
  // The sensor reported a `_last_update` that is garbage — unparseable,
  // absurdly old (Go zero time), or in the future. Shown red as "invalid":
  // a lying clock must not be trusted to claim the data is fresh.
  final bool boatTimestampInvalid;

  /// True when [level] came from a reading taken this cycle; false when it is
  /// the last known value carried forward because the boat didn't answer.
  ///
  /// Only fresh levels are recorded into the history series. A carried-forward
  /// level stamped with the current time would draw a flat line across an
  /// outage and, worse, fill the fuel rate's five-minute window with samples
  /// that were never measured — which computes as "burning nothing" and makes
  /// the rate line vanish for minutes after every reconnect.
  final bool levelIsFresh;

  Duration? get fetchedAge =>
      fetchedAt == null ? null : DateTime.now().difference(fetchedAt!);
  Duration? get boatAge =>
      boatUpdatedAt == null ? null : DateTime.now().difference(boatUpdatedAt!);
}

/// Snapshot of the live boat readings the app polls. A ChangeNotifier so the
/// UI rebuilds on each 1 Hz tick. Grows as more of the web app's readouts are
/// ported (AIS, gauges, routes, …).
class BoatState extends ChangeNotifier {
  LatLng? position;
  double? speedKn; // SOG, from linear velocity
  double? cogDeg; // course over ground
  double? headingDeg; // compass heading
  double? depthFt;
  double? seaTempF;
  double? windSpeedKn; // true wind
  double? windAngleDeg; // true wind angle
  // Boat systems (from the web app's data panel).
  double? spotZeroFwGph; // watermaker fresh-water product flow
  double? spotZeroSwGph; // watermaker sea-water flow
  bool? seakeeperStabilizing;
  bool? seakeeperPower;
  double? seakeeperProgress; // spool-up %
  List<TankStatus> tanks = const []; // fuel / fresh water
  double? acVolts; // average line-neutral voltage
  double? acWatts; // total AC load
  List<AisBoat> aisBoats = const [];
  List<String> cameraNames = const [];
  LatLng? destination; // active route destination (from the `route` sensor)
  double? wpDistanceM; // distance to next waypoint, metres
  double? wpClosingMs; // closing velocity toward the waypoint, m/s
  // Which component each reading came from (for the drawer's Sources section).
  Map<String, String?> sources = const {};
  String windInfo = 'off'; // wind-overlay fetch state, shown in Debug
  // AIS marker cull/cap counters (D10), written by the map layer builder for
  // the debug screen. Plain fields, no notify — they ride along with the
  // rebuilds that produced them.
  int aisCulled = 0;
  int aisCapped = 0;
  // Session data-budget counters (L3), plain fields for the debug screen:
  // how many poll sweeps ran and how many (heavy) AIS fetches went out.
  // Tile bytes live on TileDiskCache. Together these are the baseline
  // instrumentation for the "1 hour under way" measurement.
  int pollSweeps = 0;
  int aisFetches = 0;
  // Own-boat live track (C2), recorded as positions arrive.
  final Track track = Track();

  /// Prepend the cloud-recorded track (C3) and repaint.
  void seedTrack(List<TrackPoint> recorded) {
    track.seed(recorded);
    _notify();
  }

  // Device-GPS fallback (L5): the phone knows where it is even when the
  // boat's GPS (or the connection) is down. Phone fixes live in their own
  // field — they must never pollute the boat's track or masquerade as the
  // boat's position.
  LatLng? phonePosition;
  DateTime? lastBoatFixAt; // stamped whenever a BOAT position arrives

  static const Duration boatFixFreshFor = Duration(seconds: 15);

  bool get boatFixFresh =>
      lastBoatFixAt != null &&
      DateTime.now().difference(lastBoatFixAt!) < boatFixFreshFor;

  /// The phone's fix is standing in for the boat's.
  bool get usingPhoneGps => !boatFixFresh && phonePosition != null;

  /// Orientation to draw the boat with — web parity (src/App.svelte myBoat):
  /// under way (SOG > 1 kn) course-over-ground is the truth, since current
  /// and leeway make the compass disagree with the actual motion; below
  /// 1 kn COG is GPS noise, so the compass heading wins.
  double? get effectiveHeadingDeg =>
      ((speedKn ?? 0) > 1 && cogDeg != null) ? cogDeg : headingDeg;

  /// What the map should mark as "here": the boat's fresh fix, else the
  /// phone's, else the boat's last known.
  LatLng? get displayPosition =>
      boatFixFresh ? position : (phonePosition ?? position);

  void setPhonePosition(LatLng? p) {
    phonePosition = p;
    notifyListeners();
  }

  // Regions from `area` components (B3) — e.g. right-whale zones.
  List<AreaInfo> areas = const [];

  void setAreas(List<AreaInfo> a) {
    areas = a;
    notifyListeners();
  }

  // Nearest-station tide info (G1), refreshed by TideService.
  TideInfo? tide;

  void setTide(TideInfo t) {
    tide = t;
    notifyListeners();
  }

  // AIS position-history tracks by MMSI (D1), from the AIS sensor's
  // all_history / history DoCommands.
  Map<String, List<LatLng>> aisHistory = const {};

  /// Replace the whole history map (the 60 s all_history poll).
  void setAisHistory(Map<String, List<LatLng>> h) {
    aisHistory = h;
    notifyListeners();
  }

  /// Merge one vessel's history (fetched when its sheet opens, so the track
  /// shows immediately instead of waiting for the next poll).
  void mergeAisHistory(String mmsi, List<LatLng> points) {
    if (points.isEmpty) return;
    aisHistory = {...aisHistory, mmsi: points};
    notifyListeners();
  }

  // Active nav-service waypoints (E1/E2), refreshed by the 5 s poll and
  // replaced optimistically by edits (pending-<ts> ids until the next poll
  // brings back the backend's real ObjectIDs).
  List<NavWaypoint> navWaypoints = const [];

  void setNavWaypoints(List<NavWaypoint> wps) {
    navWaypoints = wps;
    _notify();
  }

  /// Next/Final distance + ETA over the whole remaining route (E5), or null
  /// when there's no fix or no active nav-service route.
  RouteStats? get routeStats =>
      computeRouteStats(position, navWaypoints, speedKn);

  // Per-component health from GetMachineStatus (K1): leaf component name →
  // short state ("unhealthy", "configuring", …). Only non-ready components
  // are kept, so empty means all good.
  Map<String, String> resourceHealth = const {};

  void setResourceHealth(Map<String, String> health) {
    if (mapEquals(health, resourceHealth)) return;
    resourceHealth = health;
    notifyListeners();
  }

  /// The unhealthy state for a (possibly remote-prefixed) component, or null
  /// when it's fine/unknown.
  String? healthFor(String? componentName) => componentName == null
      ? null
      : resourceHealth[componentName.split(':').last];
  /// Name of the machine this state is connected to (from the picker /
  /// remembered record) — the answer to "which boat am I looking at".
  String boatName = '';
  String status = 'Starting…';
  DateTime? lastUpdate;
  // Why the last re-dial failed, and how many attempts the current outage has
  // taken. Surfaced in Debug rather than the status pill, which is far too
  // narrow for a gRPC error.
  String? lastConnectError;
  int reconnectAttempts = 0;

  // Timestamped live history per metric (depth/seatemp/sog/wind/tank:<name>),
  // for the inline sparklines and the detail graph. ~4h at 1 Hz.
  final Map<String, List<({DateTime t, double v})>> history = {};
  static const int _histCap = 15000;

  // Metrics already backfilled from the cloud data store this session, so a
  // page re-open doesn't refetch what we're now accumulating live.
  final Set<String> backfilled = {};

  /// Prepend cloud-recorded samples (hot/cold data store) older than anything
  /// sampled live, so a graph shows real history the moment it opens; the
  /// live poll keeps appending from there. Marks [key] done even on an empty
  /// result — no point re-asking the store every page open.
  void backfill(String key, List<({DateTime t, double v})> points) {
    backfilled.add(key);
    if (points.isEmpty) return;
    final list = history.putIfAbsent(key, () => []);
    final firstLive = list.isEmpty ? null : list.first.t;
    final older = firstLive == null
        ? points
        : [
            for (final p in points)
              if (p.t.isBefore(firstLive)) p
          ];
    if (older.isEmpty) return;
    list.insertAll(0, older);
    _notify();
  }

  /// Merge cloud-recorded samples into [key]'s series ANYWHERE, not just
  /// before the first live point — the boat keeps recording while the phone
  /// is locked, so this bridges the mid-series gap a reconnect leaves.
  /// (The fuel rate needs ≥2.5 min of samples in its 5-minute window;
  /// without the bridge it blanks for minutes after every resume.) Points
  /// within 10 s of an existing sample are dropped as duplicates.
  void mergeSeries(String key, List<({DateTime t, double v})> points) {
    if (points.isEmpty) return;
    final list = history.putIfAbsent(key, () => []);
    var added = false;
    for (final p in points) {
      final dup = list.any(
          (e) => (e.t.difference(p.t)).abs() < const Duration(seconds: 10));
      if (!dup) {
        list.add(p);
        added = true;
      }
    }
    if (!added) return;
    list.sort((a, b) => a.t.compareTo(b.t));
    if (list.length > _histCap) {
      list.removeRange(0, list.length - _histCap);
    }
    _notify();
  }

  bool get connected => status.startsWith('Connected');

  /// True when there's an active waypoint to navigate to.
  bool get navigating => (wpDistanceM ?? 0) > 0;

  /// Distance to the next waypoint in nautical miles.
  double? get wpDistanceNm =>
      wpDistanceM == null ? null : wpDistanceM! * 0.000539957;

  /// Minutes to the next waypoint at the current closing velocity, or null when
  /// not closing (stationary / opening). Mirrors the web app's calc.
  double? get wpEtaMinutes =>
      (wpDistanceM != null && wpClosingMs != null && wpClosingMs! > 0)
          ? wpDistanceM! / wpClosingMs! / 60
          : null;

  /// Wall-clock arrival time, or null when ETA is unknown.
  DateTime? get wpEta {
    final m = wpEtaMinutes;
    return m == null
        ? null
        : DateTime.now().add(Duration(seconds: (m * 60).round()));
  }

  /// Last ~60 values for the inline sparkline glance.
  List<double> spark(String key) {
    final l = history[key];
    if (l == null) return const [];
    final start = l.length > 60 ? l.length - 60 : 0;
    return [for (var i = start; i < l.length; i++) l[i].v];
  }

  /// Full timestamped series for the detail graph.
  List<({DateTime t, double v})> series(String key) =>
      history[key] ?? const [];

  void _push(String key, double v) {
    final list = history.putIfAbsent(key, () => []);
    list.add((t: DateTime.now(), v: v));
    if (list.length > _histCap) list.removeAt(0);
  }

  void setAis(List<AisBoat> boats) {
    aisBoats = boats;
    _notify();
  }

  void setRoute({
    LatLng? destination,
    double? wpDistanceM,
    double? wpClosingMs,
  }) {
    this.destination = destination;
    this.wpDistanceM = wpDistanceM;
    this.wpClosingMs = wpClosingMs;
    _notify();
  }

  void setSources(Map<String, String?> s) {
    sources = s;
    _notify();
  }

  void setCameras(List<String> names) {
    cameraNames = names;
    _notify();
  }

  void setWindInfo(String s) {
    windInfo = s;
    _notify();
  }

  void setSystems({
    double? spotZeroFwGph,
    double? spotZeroSwGph,
    bool? seakeeperStabilizing,
    bool? seakeeperPower,
    double? seakeeperProgress,
    List<TankStatus>? tanks,
    double? acVolts,
    double? acWatts,
  }) {
    if (spotZeroFwGph != null) this.spotZeroFwGph = spotZeroFwGph;
    if (spotZeroSwGph != null) this.spotZeroSwGph = spotZeroSwGph;
    if (seakeeperStabilizing != null) {
      this.seakeeperStabilizing = seakeeperStabilizing;
    }
    if (seakeeperPower != null) this.seakeeperPower = seakeeperPower;
    if (seakeeperProgress != null) this.seakeeperProgress = seakeeperProgress;
    if (tanks != null) {
      this.tanks = tanks;
      for (final t in tanks) {
        final lvl = t.level;
        if (lvl != null && t.levelIsFresh) _push('tank:${t.name}', lvl);
      }
    }
    if (acVolts != null) this.acVolts = acVolts;
    if (acWatts != null) this.acWatts = acWatts;
    _notify();
  }

  void setStatus(String s) {
    if (s == status) return; // the reconnect countdown re-asserts a lot
    status = s;
    _notify();
  }

  void setConnectionDiag({required String? error, required int attempts}) {
    if (error == lastConnectError && attempts == reconnectAttempts) return;
    lastConnectError = error;
    reconnectAttempts = attempts;
    _notify();
  }

  bool _disposed = false;

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  /// A poll abandoned mid-flight, or a re-dial that landed after "switch
  /// boat", can still call in after this state was retired — swallow those
  /// instead of tripping ChangeNotifier's use-after-dispose assert.
  void _notify() {
    if (_disposed) return;
    notifyListeners();
  }

  void update({
    LatLng? position,
    double? speedKn,
    double? cogDeg,
    double? headingDeg,
    double? depthFt,
    double? seaTempF,
    double? windSpeedKn,
    double? windAngleDeg,
  }) {
    if (position != null) {
      this.position = position;
      lastBoatFixAt = DateTime.now();
      // Live track (C2): the Track applies its own minimum-move filter, so
      // a moored boat doesn't accumulate points.
      track.record(position, depthFt: depthFt ?? this.depthFt);
    }
    if (speedKn != null) {
      this.speedKn = speedKn;
      _push('sog', speedKn);
    }
    if (cogDeg != null) this.cogDeg = cogDeg;
    if (headingDeg != null) this.headingDeg = headingDeg;
    if (depthFt != null) {
      this.depthFt = depthFt;
      _push('depth', depthFt);
    }
    if (seaTempF != null) {
      this.seaTempF = seaTempF;
      _push('seatemp', seaTempF);
    }
    if (windSpeedKn != null) {
      this.windSpeedKn = windSpeedKn;
      _push('wind', windSpeedKn);
    }
    if (windAngleDeg != null) this.windAngleDeg = windAngleDeg;
    lastUpdate = DateTime.now();
    _notify();
  }
}
