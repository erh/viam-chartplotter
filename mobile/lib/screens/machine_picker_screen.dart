import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

import '../auth/token_store.dart';
import '../auth/viam_session.dart';

/// After login, walk the user's org → location → machine and open a
/// RobotClient for the chosen boat via `viam.getRobotClient`. Hands the
/// connected client back through [onConnected].
class MachinePickerScreen extends StatefulWidget {
  const MachinePickerScreen({
    super.key,
    required this.session,
    required this.onConnected,
    this.autoConnect = true,
  });

  final ViamSession session;
  final void Function(RobotClient robot) onConnected;

  /// Reconnect to the last-connected machine automatically on open. The
  /// "switch boat" flow passes false so the user actually gets the list.
  final bool autoConnect;

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
  String _connectingName = ''; // shown while dialing ('' = no name known)
  // Bumped on every connect attempt and on cancel; an in-flight dial whose
  // generation is stale closes its client instead of taking the screen, so
  // cancel + pick-another can't race into two live connections.
  int _connectGen = 0;
  String _orgId = ''; // org of the machine being connected (set on org tap)
  final TokenStore _storage = TokenStore();

  Viam get _viam => widget.session.viam!;

  @override
  void initState() {
    super.initState();
    _loadOrgs();
    if (widget.autoConnect) _tryAutoConnect();
  }

  /// Reconnect to the machine the user was on last run, so launching the app
  /// on the boat goes straight to the map. The org/machine list keeps
  /// loading behind it, so "Pick a different boat" is instant.
  Future<void> _tryAutoConnect() async {
    try {
      final raw = await _storage.read(key: _kLastMachine);
      if (raw == null) return;
      final m = jsonDecode(raw) as Map<String, dynamic>;
      final robotId = m['robotId'];
      final orgId = m['orgId'];
      if (robotId is! String || orgId is! String) return;
      if (!mounted || _connecting) return;
      final gen = ++_connectGen;
      setState(() {
        _connecting = true;
        _connectingName = (m['name'] is String) ? m['name'] as String : '';
      });
      _orgId = orgId;
      final client = await _connectRobot(robotId);
      if (gen != _connectGen) {
        await client.close();
        return;
      }
      if (mounted) widget.onConnected(client);
    } catch (_) {
      // Stale machine, revoked access, offline — fall back to the list.
      if (mounted && _connecting) {
        setState(() {
          _connecting = false;
          _connectingName = '';
        });
      }
    }
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
        _orgId = org.id;
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
    final gen = ++_connectGen;
    setState(() {
      _connecting = true;
      _connectingName = robot.name?.toString() ?? '';
    });
    try {
      final client = await _connectRobot(robot.id);
      if (gen != _connectGen) {
        await client.close();
        return;
      }
      // Remember this machine so the next launch can reconnect to it
      // without walking the picker.
      try {
        await _storage.write(
          key: _kLastMachine,
          value: jsonEncode({
            'robotId': robot.id,
            'orgId': _orgId,
            'name': robot.name?.toString() ?? '',
          }),
        );
      } catch (_) {}
      widget.onConnected(client);
    } catch (e) {
      if (mounted && gen == _connectGen) {
        setState(() {
          _connecting = false;
          _connectingName = '';
          _error = 'Connect failed: $e';
        });
      }
    }
  }

  /// Dial the machine's main part via app.viam.com signaling with a
  /// per-machine robot-owner API key, minted through the app API on first
  /// connect and cached (re-minted once if the stored key stops working,
  /// one retry on a dial timeout). Machine API keys are the credential
  /// signaling reliably accepts: it hangs on our OAuth app's access token,
  /// and robot secrets are deprecated (empty/disabled on newer machines).
  Future<RobotClient> _connectRobot(String robotId) async {
    final parts = await _viam.appClient.listRobotParts(robotId);
    final part = parts.firstWhere((p) => p.mainPart);
    // NOTE: no local/mDNS attempt. viam-server's `_rpc._tcp` advertisement
    // here is its internal signaling listener bound to the MACHINE'S OWN
    // loopback (utils/rpc server.go registers it with a 127.0.0.1 A-record
    // and an ephemeral port that changes on restart) — usable only by
    // clients on the boat itself, never from another device, so the Dart
    // SDK's mDNS dial can't succeed from here. Attempting it also risks the
    // SDK's poisoned-fallback bug (dial() mutates its options on the mDNS
    // try, so its internal fallback redials the same dead endpoint) plus a
    // robot-secret auth that hangs where the deprecated secret is disabled.
    // Cloud signaling is only the handshake — ICE still picks a direct LAN
    // path when both peers are on the boat network.
    final stored = await _loadStoredKey(robotId);
    if (stored != null) {
      try {
        return await _dialWithKey(part.fqdn, stored);
      } catch (_) {
        // Key revoked/rotated server-side — discard and mint a fresh one.
        try {
          await _storage.delete(key: _storageKey(robotId));
        } catch (_) {}
      }
    }
    final minted = await _mintKey(robotId);
    try {
      return await _dialWithKey(part.fqdn, minted);
    } on TimeoutException {
      // Signaling/answerer hiccups showed up as one-off 10 s dial timeouts
      // during testing while the identical dial succeeded moments later —
      // one retry before surfacing the error.
      return await _dialWithKey(part.fqdn, minted);
    }
  }

  static const _kLastMachine = 'last_machine';
  static String _storageKey(String robotId) => 'machine_api_key_$robotId';

  Future<({String id, String key})?> _loadStoredKey(String robotId) async {
    try {
      final raw = await _storage.read(key: _storageKey(robotId));
      if (raw == null) return null;
      final m = jsonDecode(raw) as Map<String, dynamic>;
      final id = m['id'];
      final key = m['key'];
      if (id is String && key is String) return (id: id, key: key);
    } catch (_) {
      // Unreadable storage or corrupt entry — treat as no stored key.
    }
    return null;
  }

  Future<({String id, String key})> _mintKey(String robotId) async {
    final resp = await _viam.appClient.createKey(
      [
        ViamAuthorization(
          authorizationId: AuthorizationId.robotOwner,
          resourceType: ResourceType.robot,
          resourceId: robotId,
          organizationId: _orgId,
          identityType: IdentityType.apiKey,
        ),
      ],
      'chartplotter-mobile',
    );
    try {
      await _storage.write(
        key: _storageKey(robotId),
        value: jsonEncode({'id': resp.id, 'key': resp.key}),
      );
    } catch (_) {
      // Storage unavailable — the key still works for this run.
    }
    return (id: resp.id, key: resp.key);
  }

  Future<RobotClient> _dialWithKey(String fqdn, ({String id, String key}) k) {
    final options = RobotClientOptions.withApiKey(k.id, k.key)
      ..dialOptions.attemptMdns = false;
    return RobotClient.atAddress(fqdn, options);
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
          ? Center(
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  const CircularProgressIndicator(),
                  const SizedBox(height: 16),
                  Text(_connectingName.isEmpty
                      ? 'Connecting to boat…'
                      : 'Connecting to $_connectingName…'),
                  const SizedBox(height: 16),
                  OutlinedButton(
                    onPressed: () {
                      // Back out to the list (loaded in the background); if
                      // the in-flight connect still lands, its stale
                      // generation closes it instead of taking the screen.
                      _connectGen++;
                      setState(() {
                        _connecting = false;
                        _connectingName = '';
                      });
                    },
                    child: const Text('Pick a different boat'),
                  ),
                ],
              ),
            )
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
