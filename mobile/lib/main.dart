import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'auth/oauth_config.dart';
import 'auth/viam_session.dart';
import 'boat_state.dart';
import 'map_screen.dart';
import 'screens/login_screen.dart';
import 'screens/machine_picker_screen.dart';
import 'viam_connection.dart';

void main() {
  runApp(const ChartplotterApp());
}

class ChartplotterApp extends StatefulWidget {
  const ChartplotterApp({super.key});

  @override
  State<ChartplotterApp> createState() => _ChartplotterAppState();
}

class _ChartplotterAppState extends State<ChartplotterApp> {
  final BoatState _state = BoatState();
  final ViamSession _session = ViamSession();
  late final ViamConnection _conn = ViamConnection(_state);

  // True once we have a live RobotClient (either via login→picker or the
  // API-key fallback), meaning we can show the map.
  bool _boatConnected = false;

  @override
  void initState() {
    super.initState();
    _session.addListener(_onSession);
    _bootstrap();
  }

  Future<void> _bootstrap() async {
    if (OAuthConfig.configured) {
      // Login-based path: restore any stored session, then let the widget tree
      // route to login / picker / map based on auth status.
      await _session.restore();
    } else {
      // No OAuth configured → preserve the spike's API-key path so the app
      // still runs. Connects straight to the configured boat (or chart-only).
      await _conn.startWithApiKey();
      if (mounted) setState(() => _boatConnected = true);
    }
  }

  void _onSession() {
    if (mounted) setState(() {});
  }

  void _onRobotConnected(RobotClient robot) {
    _conn.startWithRobot(robot);
    setState(() => _boatConnected = true);
  }

  @override
  void dispose() {
    _session.removeListener(_onSession);
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
        if (_boatConnected) return MapScreen(state: _state, connection: _conn);
        return MachinePickerScreen(
          session: _session,
          onConnected: _onRobotConnected,
        );
    }
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Viam Chartplotter',
      debugShowCheckedModeBanner: false,
      theme: ThemeData.dark(useMaterial3: true),
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
