import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'auth/oauth_config.dart';
import 'auth/token_store.dart';
import 'auth/viam_session.dart';
import 'boat_state.dart';
import 'connect.dart';
import 'app_config.dart';
import 'config.dart';
import 'map/tile_cache.dart';
import 'map_screen.dart';
import 'phone_gps.dart';
import 'tides.dart';
import 'screens/login_screen.dart';
import 'screens/machine_picker_screen.dart';
import 'settings.dart';
import 'viam_connection.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  // Restore persisted map/tool state before the first frame so the map opens
  // exactly where it was left, with no visible jump (J6).
  await Settings.init();
  // First launch: seed the safe depth from the --dart-define, if given (A2).
  if (Settings.instance.safeDepthFt == null && Config.safeDepthFt.isNotEmpty) {
    Settings.instance.safeDepthFt = int.tryParse(Config.safeDepthFt);
  }
  // Tile disk cache (L1): previously-seen water renders without network,
  // including fully offline. Failure just means uncached tiles.
  await TileDiskCache.init();
  runApp(const ChartplotterApp());
}

// Note: AppConfig.load() runs from _bootstrap, not here — it can take up to
// its 5 s timeout when the module server is unreachable, and the map should
// paint immediately on the cached/default base meanwhile (A6).

class ChartplotterApp extends StatefulWidget {
  const ChartplotterApp({super.key});

  @override
  State<ChartplotterApp> createState() => _ChartplotterAppState();
}

class _ChartplotterAppState extends State<ChartplotterApp>
    with WidgetsBindingObserver {
  BoatState _state = BoatState();
  final ViamSession _session = ViamSession();
  late ViamConnection _conn = ViamConnection(_state);
  // Device-GPS fallback (L5): starts with the boat connection, watches for
  // the boat fix going stale, and lazily asks for location permission.
  late PhoneGps _phoneGps = PhoneGps(_state);
  // Nearest-station tides (G1); refreshes as the boat moves.
  late TideService _tides = TideService(_state);

  // True once we have a live RobotClient (either via login→picker or the
  // API-key fallback), meaning we can show the map.
  bool _boatConnected = false;

  // True right after "switch boat": the picker should show the list instead
  // of auto-reconnecting to the boat the user just left.
  bool _skipAutoConnect = false;

  // When the app was last backgrounded, so resume can tell a glance at a
  // notification from a stint in a pocket.
  DateTime? _pausedAt;

  /// Past this much time backgrounded, assume the WebRTC peer is gone and
  /// re-dial on resume without spending a probe deadline confirming it.
  static const _deadAfterBackground = Duration(seconds: 30);

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    _session.addListener(_onSession);
    _bootstrap();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    // A restore that failed only because we were offline at launch should heal
    // itself once the app comes back with a network, rather than sitting on
    // the login screen waiting for a tap.
    if (state == AppLifecycleState.resumed && _session.canRetryRestore) {
      _session.retryRestore();
    }
    if (!_boatConnected) return;
    // Locking the phone kills the WebRTC connection but leaves the app
    // showing its last status. Stop polling while we're away (the OS suspends
    // or throttles us anyway, and ticking just burns battery and cellular
    // data), then settle the connection's fate the moment we're back instead
    // of letting it silently lie until the watchdog notices.
    if (state == AppLifecycleState.resumed) {
      final away = _pausedAt;
      _pausedAt = null;
      final gone = away != null &&
          DateTime.now().difference(away) > _deadAfterBackground;
      _conn.resume(assumeDead: gone);
    } else if (state == AppLifecycleState.paused ||
        state == AppLifecycleState.hidden) {
      _pausedAt ??= DateTime.now();
      _conn.pause();
    }
  }

  Future<void> _bootstrap() async {
    if (OAuthConfig.configured) {
      // Login-based path: restore any stored session, then let the widget tree
      // route to login / picker / map based on auth status. /app-config is
      // probed in the background — the map paints on the cached/default tile
      // base immediately and switches live if the server names another (A6).
      unawaited(AppConfig.load());
      await _session.restore();
    } else {
      // No OAuth configured → the spike's API-key path. Await the config
      // probe here: a server declaring chartOnly means show the chart and
      // poll no boat at all (A6).
      await AppConfig.load();
      if (AppConfig.chartOnly) {
        _state.setStatus('Chart-only server');
      } else {
        await _conn.startWithApiKey();
        _phoneGps.start();
        _tides.start();
      }
      if (mounted) setState(() => _boatConnected = true);
    }
  }

  void _onSession() {
    if (mounted) setState(() {});
  }

  void _onRobotConnected(RobotClient robot, String robotId, String name) {
    _state.boatName = name;
    // robotId lets the connection fetch the machine config once and honour
    // chartplotter-hide during discovery (H6).
    _conn.startWithRobot(robot, viam: _session.viam, robotId: robotId);
    // Dead-connection recovery: re-dial the machine we're on via the shared
    // connect path (cached machine API key), using the picker's stored
    // last-machine record.
    _conn.reconnector = () async {
      // Refresh the OAuth session first. A re-dial still needs app.viam.com
      // (to verify or mint the machine key), so an access token that expired
      // while the app was backgrounded used to turn every retry into a
      // permanent failure — the app would sit on "reconnecting…" forever with
      // no way out but a restart.
      await _session.validAccessToken();
      final viam = _session.viam;
      final raw = await TokenStore().read(key: kLastMachineKey);
      if (viam == null || raw == null) {
        throw StateError('no session or machine to reconnect to');
      }
      final m = jsonDecode(raw) as Map<String, dynamic>;
      final cached = m['fqdn'];
      return MachineConnector(viam: viam).connect(
        m['robotId'] as String,
        m['orgId'] as String,
        cachedFqdn: cached is String && cached.isNotEmpty ? cached : null,
      );
    };
    _phoneGps.start();
    _tides.start();
    setState(() {
      _boatConnected = true;
      _skipAutoConnect = false;
    });
  }

  /// "Switch boat": drop the live connection and return to the picker with
  /// fresh state (so the old boat's readings don't linger on the new one).
  void _onSwitchBoat() {
    final oldConn = _conn;
    final oldState = _state;
    unawaited(_phoneGps.stop());
    unawaited(_tides.stop());
    _state = BoatState();
    _conn = ViamConnection(_state);
    _phoneGps = PhoneGps(_state);
    _tides = TideService(_state);
    setState(() {
      _boatConnected = false;
      _skipAutoConnect = true;
    });
    oldConn.stop().then((_) => oldState.dispose());
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _session.removeListener(_onSession);
    unawaited(_phoneGps.stop());
    unawaited(_tides.stop());
    _conn.dispose();
    _session.dispose();
    _state.dispose();
    super.dispose();
  }

  Widget _home() {
    // API-key / chart-only path.
    if (!OAuthConfig.configured) {
      return MapScreen(state: _state, connection: _conn);
    }
    // Login path.
    switch (_session.status) {
      case AuthStatus.unknown:
        return const _Splash();
      case AuthStatus.signedOut:
      case AuthStatus.signingIn:
      case AuthStatus.error:
        return LoginScreen(session: _session);
      case AuthStatus.signedIn:
        if (_boatConnected) {
          return MapScreen(
            state: _state,
            connection: _conn,
            onSwitchBoat: _onSwitchBoat,
          );
        }
        return MachinePickerScreen(
          session: _session,
          onConnected: _onRobotConnected,
          autoConnect: !_skipAutoConnect,
        );
    }
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Viam Chartplotter',
      debugShowCheckedModeBanner: false,
      theme: ThemeData.dark(useMaterial3: true),
      // Clamp system text scaling (Dynamic Type / Android font size).
      // Chart chrome is spatial instrument UI: pills, dialogs, and the
      // drawer are laid out around the map and break outright at the
      // accessibility sizes (3x+). Up to 1.3x still honours "larger text"
      // for readability; past that, layouts hold instead of overflowing —
      // the same call every marine plotter and instrument cluster makes.
      builder: (context, child) => MediaQuery.withClampedTextScaling(
        minScaleFactor: 1.0,
        maxScaleFactor: 1.3,
        child: child ?? const SizedBox.shrink(),
      ),
      home: _home(),
    );
  }
}

class _Splash extends StatelessWidget {
  const _Splash();
  @override
  Widget build(BuildContext context) =>
      const Scaffold(body: Center(child: CircularProgressIndicator()));
}
