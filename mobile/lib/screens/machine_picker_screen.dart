import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

import '../auth/viam_session.dart';

/// After login, walk the user's org → location → machine and open a
/// RobotClient for the chosen boat via `viam.getRobotClient`. Hands the
/// connected client back through [onConnected].
class MachinePickerScreen extends StatefulWidget {
  const MachinePickerScreen({
    super.key,
    required this.session,
    required this.onConnected,
  });

  final ViamSession session;
  final void Function(RobotClient robot) onConnected;

  @override
  State<MachinePickerScreen> createState() => _MachinePickerScreenState();
}

enum _Level { orgs, locations, robots }

class _MachinePickerScreenState extends State<MachinePickerScreen> {
  _Level _level = _Level.orgs;
  List<dynamic> _items = [];
  bool _loading = true;
  String? _error;
  String _title = 'Select organization';
  bool _connecting = false;

  Viam get _viam => widget.session.viam!;

  @override
  void initState() {
    super.initState();
    _loadOrgs();
  }

  Future<void> _guard(Future<void> Function() body) async {
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      await body();
    } catch (e) {
      _error = '$e';
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  Future<void> _loadOrgs() => _guard(() async {
        final orgs = await _viam.appClient.listOrganizations();
        setState(() {
          _level = _Level.orgs;
          _title = 'Select organization';
          _items = orgs;
        });
      });

  Future<void> _loadLocations(dynamic org) => _guard(() async {
        final locs = await _viam.appClient.listLocations(org.id);
        setState(() {
          _level = _Level.locations;
          _title = 'Select location';
          _items = locs;
        });
      });

  Future<void> _loadRobots(dynamic loc) => _guard(() async {
        final robots = await _viam.appClient.listRobots(loc.id);
        setState(() {
          _level = _Level.robots;
          _title = 'Select machine';
          _items = robots;
        });
      });

  Future<void> _connect(dynamic robot) async {
    setState(() => _connecting = true);
    try {
      final client = await _connectRobot(robot);
      widget.onConnected(client);
    } catch (e) {
      if (mounted) {
        setState(() {
          _connecting = false;
          _error = 'Connect failed: $e';
        });
      }
    }
  }

  /// Connect to the machine's main part like `viam.getRobotClient` does, but
  /// with mDNS disabled. The SDK otherwise tries the machine's `<fqdn>.local`
  /// endpoint on the LAN first and, when that direct dial is refused, does NOT
  /// fall back to the cloud — so a connect that works in the browser (which has
  /// no mDNS) fails on native. Skipping mDNS uses the same app.viam.com WebRTC
  /// path the web app uses, which works from anywhere.
  Future<RobotClient> _connectRobot(dynamic robot) async {
    final parts = await _viam.appClient.listRobotParts(robot.id);
    final part = parts.firstWhere((p) => p.mainPart);
    final options = RobotClientOptions.withRobotSecret(part.secret);
    options.dialOptions.attemptMdns = false;
    return RobotClient.atAddress(part.fqdn, options);
  }

  void _onTap(dynamic item) {
    switch (_level) {
      case _Level.orgs:
        _loadLocations(item);
      case _Level.locations:
        _loadRobots(item);
      case _Level.robots:
        _connect(item);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: Text(_title),
        actions: [
          IconButton(
            tooltip: 'Sign out',
            onPressed: widget.session.signOut,
            icon: const Icon(Icons.logout),
          ),
        ],
      ),
      body: _connecting
          ? const Center(child: Text('Connecting to boat…'))
          : _loading
              ? const Center(child: CircularProgressIndicator())
              : _error != null
                  ? _ErrorRetry(message: _error!, onRetry: _loadOrgs)
                  : ListView.separated(
                      itemCount: _items.length,
                      separatorBuilder: (_, __) => const Divider(height: 1),
                      itemBuilder: (context, i) {
                        final item = _items[i];
                        return ListTile(
                          leading: Icon(_level == _Level.robots
                              ? Icons.sailing
                              : Icons.folder_outlined),
                          title: Text(item.name?.toString() ?? '(unnamed)'),
                          trailing: Icon(_level == _Level.robots
                              ? Icons.link
                              : Icons.chevron_right),
                          onTap: () => _onTap(item),
                        );
                      },
                    ),
    );
  }
}

class _ErrorRetry extends StatelessWidget {
  const _ErrorRetry({required this.message, required this.onRetry});
  final String message;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    return Center(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Padding(
            padding: const EdgeInsets.all(24),
            child: SelectableText(message, textAlign: TextAlign.center),
          ),
          OutlinedButton(onPressed: onRetry, child: const Text('Retry')),
        ],
      ),
    );
  }
}
