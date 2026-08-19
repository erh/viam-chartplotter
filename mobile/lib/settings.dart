import 'dart:convert';

import 'package:latlong2/latlong.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// Persisted map/tool state (task J6) — a typed, validating wrapper over
/// shared_preferences, so every launch resumes where the last one left off
/// instead of the hardcoded Long Island Sound view.
///
/// Keys reuse the web app's cookie names where the value means the same thing
/// (`mapViewCenter`, `mapViewZoom`, `mapHeadsUp`; see
/// src/marineMap.svelte:272-284) purely to make cross-referencing the two
/// clients easy — there is no shared storage. Every getter validates and
/// falls back to its default on garbage, like the web loaders do.
///
/// [Settings.init] must complete before the first frame (main() awaits it) so
/// restores never cause a visible jump.
class Settings {
  Settings._(this._prefs, this.buildStamp);

  static Settings? _instance;
  static Settings get instance => _instance!;

  final SharedPreferences _prefs;

  /// `<version>+<build>` for the tile cache-buster (A3) — changes on every
  /// release, so a new build never serves stale chart renders from an HTTP
  /// (or future disk) cache.
  final String buildStamp;

  static Future<Settings> init() async {
    final prefs = await SharedPreferences.getInstance();
    String stamp = 'dev';
    try {
      final info = await PackageInfo.fromPlatform();
      stamp = '${info.version}+${info.buildNumber}';
    } catch (_) {
      // Platform channel unavailable (tests) — keep the dev stamp.
    }
    return _instance = Settings._(prefs, stamp);
  }

  /// Test seam: install an instance backed by in-memory prefs.
  static void setForTesting(SharedPreferences prefs, {String stamp = 'test'}) {
    _instance = Settings._(prefs, stamp);
  }

  // --- map view --------------------------------------------------------

  /// Last map center as `[lon, lat]` JSON (web: `mapViewCenter`).
  LatLng? get mapCenter {
    final raw = _prefs.getString('mapViewCenter');
    if (raw == null) return null;
    try {
      final v = jsonDecode(raw);
      if (v is List && v.length == 2) {
        final lon = (v[0] as num).toDouble();
        final lat = (v[1] as num).toDouble();
        if (lat.abs() <= 90 && lon.abs() <= 180) return LatLng(lat, lon);
      }
    } catch (_) {}
    return null;
  }

  set mapCenter(LatLng? c) {
    if (c == null) {
      _prefs.remove('mapViewCenter');
    } else {
      _prefs.setString('mapViewCenter', jsonEncode([c.longitude, c.latitude]));
    }
  }

  /// Web: `mapViewZoom`, validated 0 < z <= 22.
  double? get mapZoom {
    final z = _prefs.getDouble('mapViewZoom');
    return (z != null && z > 0 && z <= 22) ? z : null;
  }

  set mapZoom(double? z) {
    if (z == null || z <= 0 || z > 22) {
      _prefs.remove('mapViewZoom');
    } else {
      _prefs.setDouble('mapViewZoom', z);
    }
  }

  /// Course-up vs north-up (web: `mapHeadsUp`, "1"/"0").
  bool get courseUp => _prefs.getString('mapHeadsUp') == '1';
  set courseUp(bool v) => _prefs.setString('mapHeadsUp', v ? '1' : '0');

  /// Selected base layer id; callers validate it against the known layers.
  String? get baseLayerId => _prefs.getString('baseLayer');
  set baseLayerId(String? id) => id == null
      ? _prefs.remove('baseLayer')
      : _prefs.setString('baseLayer', id);

  // --- tools -----------------------------------------------------------

  bool get windOn => _prefs.getBool('windOn') ?? false;
  set windOn(bool v) => _prefs.setBool('windOn', v);

  /// Colour the own-boat track by recorded depth (C2): shoal transits red.
  bool get depthColorTrack => _prefs.getBool('depthColorTrack') ?? false;
  set depthColorTrack(bool v) => _prefs.setBool('depthColorTrack', v);

  /// Where the boat sits on screen while following (J2): centred, or 80%
  /// down the screen — look-ahead mode, what you want under way. Web key
  /// `mapBoatPosition` ("center"/"bottom").
  bool get boatPositionBottom => _prefs.getString('mapBoatPosition') == 'bottom';
  set boatPositionBottom(bool v) =>
      _prefs.setString('mapBoatPosition', v ? 'bottom' : 'center');

  /// Web-sender AIS targets (D6): rebroadcast positions from ais-web-sender
  /// components, togglable like the web app's "web senders" layer checkbox.
  bool get webSendersOn => _prefs.getBool('webSendersOn') ?? true;
  set webSendersOn(bool v) => _prefs.setBool('webSendersOn', v);

  /// Low data mode (L3): stretch every poll cadence and lean on the tile
  /// cache — for metered cellular/satellite links offshore.
  bool get lowDataOn => _prefs.getBool('lowDataOn') ?? false;
  set lowDataOn(bool v) => _prefs.setBool('lowDataOn', v);

  /// Selected wind-forecast model (F2), e.g. "gfs" / "hrrr".
  String? get windModel => _prefs.getString('windModel');
  set windModel(String? v) =>
      v == null ? _prefs.remove('windModel') : _prefs.setString('windModel', v);

  /// Night mode (L4): red-multiply the whole chart UI to protect night
  /// vision. The server has no night render, so this is client-side.
  bool get nightModeOn => _prefs.getBool('nightModeOn') ?? false;
  set nightModeOn(bool v) => _prefs.setBool('nightModeOn', v);

  /// Helm mode (L4): keep the screen awake while the chart is foregrounded.
  /// Explicitly opt-in — always-on would silently eat the battery ashore.
  bool get keepAwakeOn => _prefs.getBool('keepAwakeOn') ?? false;
  set keepAwakeOn(bool v) => _prefs.setBool('keepAwakeOn', v);

  /// AIS history tracks (D1): gates both the drawing and the 60 s
  /// all_history poll (web: the ais-track layer / aisTracksNeeded, default
  /// off).
  bool get aisTracksOn => _prefs.getBool('aisTracksOn') ?? false;
  set aisTracksOn(bool v) => _prefs.setBool('aisTracksOn', v);

  /// AIS/own-boat projection vectors (D2): minutes ahead along COG at SOG.
  /// Web key `mapAisProjectionMin`, one of [1, 2, 5, 10], default 2.
  int get aisProjectionMin {
    final v = _prefs.getInt('mapAisProjectionMin');
    return (v != null && const [1, 2, 5, 10].contains(v)) ? v : 2;
  }

  set aisProjectionMin(int v) => _prefs.setInt('mapAisProjectionMin', v);

  /// Heading line (C4): shown by default, length one of the web app's
  /// choices (web: `mapHeadingLineLengthNm`, default 5).
  bool get headingLineOn => _prefs.getBool('headingLineOn') ?? true;
  set headingLineOn(bool v) => _prefs.setBool('headingLineOn', v);

  int get headingLineLengthNm {
    final v = _prefs.getInt('mapHeadingLineLengthNm');
    return (v != null && const [1, 2, 3, 5, 10, 15].contains(v)) ? v : 5;
  }

  set headingLineLengthNm(int v) =>
      _prefs.setInt('mapHeadingLineLengthNm', v);

  /// Tile server base resolved from /app-config (A6), cached so the next
  /// launch starts on it without re-probing. Null = use the built-in default.
  String? get tileBaseOverride {
    final v = _prefs.getString('tileServerBaseURL');
    return (v != null && (v.startsWith('http://') || v.startsWith('https://')))
        ? v
        : null;
  }

  set tileBaseOverride(String? v) => v == null
      ? _prefs.remove('tileServerBaseURL')
      : _prefs.setString('tileServerBaseURL', v);

  /// Safe depth in feet for the chart's DEPARE shading (A2). Null = never
  /// set → the tile URL omits `sd` and the server default applies.
  int? get safeDepthFt {
    final v = _prefs.getInt('safeDepthFt');
    return (v != null && v > 0 && v <= 200) ? v : null;
  }

  set safeDepthFt(int? v) {
    if (v == null || v <= 0 || v > 200) {
      _prefs.remove('safeDepthFt');
    } else {
      _prefs.setInt('safeDepthFt', v);
    }
  }
}
