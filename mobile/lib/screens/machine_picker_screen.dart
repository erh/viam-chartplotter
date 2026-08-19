import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

import '../auth/token_store.dart';
import '../auth/viam_session.dart';
import '../connect.dart';

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
  final void Function(RobotClient robot, String robotId) onConnected;

  /// Reconnect to the last-connected machine automatically on open. The
  /// "switch boat" flow passes false so the user actually gets the list.
  final bool autoConnect;

  @override
  State<MachinePickerScreen> createState() => _MachinePickerScreenState();
}

enum _Level { orgs, locations, robots }

/// Case-insensitive name filter for the picker's search box: each page
/// filters ITS OWN list (orgs on the org page, locations on the location
/// page, machines on the machine page). A blank query filters nothing.
/// Pure, for tests. (Cross-org and org-wide boat search were both tried
/// and rejected: walking every location was far too slow on real accounts.)
List<dynamic> filterByName(List<dynamic> items, String query) {
  final q = query.trim().toLowerCase();
  if (q.isEmpty) return items;
  return [
    for (final it in items)
      if ((it.name?.toString().toLowerCase() ?? '').contains(q)) it
  ];
}

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
  String? _lastFqdn; // address the last dial resolved, cached in the record
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
      final raw = await _storage.read(key: kLastMachineKey);
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
      final cached = m['fqdn'];
      final client = await _connectRobot(
        robotId,
        cachedFqdn: cached is String && cached.isNotEmpty ? cached : null,
      );
      if (gen != _connectGen) {
        await client.close();
        return;
      }
      // Records written before the address was cached get one now, so the
      // first re-dial after a dropout can skip the cloud lookup.
      if (cached is! String || cached != _lastFqdn) {
        await _rememberMachine(
          robotId: robotId,
          orgId: orgId,
          name: (m['name'] is String) ? m['name'] as String : '',
        );
      }
      if (mounted) widget.onConnected(client, robotId);
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
      // Renew the session before any cloud call: the access token can expire
      // while the app sits on this screen, and an unauthenticated failure
      // reads to the user as "the app broke".
      await widget.session.validAccessToken();
      if (widget.session.viam == null) return; // signed out; the tree reroutes
      await body();
    } catch (e) {
      _error = '$e';
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  // Sorted by name so a long org/machine list is scannable — the API returns
  // them in no particular order, which read as "my org is missing" — and
  // deduped by id, since an org can come back twice (e.g. two membership
  // paths to it).
  static List<dynamic> _byName(List<dynamic> items) {
    final seen = <String>{};
    final unique = [
      for (final it in items)
        if (seen.add(it.id?.toString() ?? identityHashCode(it).toString())) it
    ];
    return unique
      ..sort((a, b) => (a.name?.toString().toLowerCase() ?? '')
          .compareTo(b.name?.toString().toLowerCase() ?? ''));
  }

  // Selection path, so back/refresh can rebuild the current or parent level.
  dynamic _selectedOrg;
  dynamic _selectedLoc;

  // ---- search box (per page) ---------------------------------------------
  // Filters whatever list is showing; nothing is fetched beyond it.
  final TextEditingController _searchCtl = TextEditingController();
  String _query = '';

  /// A level change means a different list — a stale query must not filter
  /// (or hide) it invisibly.
  void _clearSearch() {
    _searchCtl.clear();
    _query = '';
  }

  @override
  void dispose() {
    _searchCtl.dispose();
    super.dispose();
  }

  Future<void> _loadOrgs() => _guard(() async {
        final orgs = await _viam.appClient.listOrganizations();
        setState(() {
          _level = _Level.orgs;
          _title = 'Select organization';
          _items = _byName(orgs);
          _clearSearch();
        });
      });

  Future<void> _loadLocations(dynamic org) => _guard(() async {
        _orgId = org.id;
        _selectedOrg = org;
        final locs = await _viam.appClient.listLocations(org.id);
        setState(() {
          _level = _Level.locations;
          _title = 'Select location';
          _items = _byName(locs);
          _clearSearch();
        });
      });

  Future<void> _loadRobots(dynamic loc) => _guard(() async {
        _selectedLoc = loc;
        final robots = await _viam.appClient.listRobots(loc.id);
        setState(() {
          _level = _Level.robots;
          _title = 'Select machine';
          _items = _byName(robots);
          _clearSearch();
        });
      });

  /// Reload whatever level is showing (pull-to-refresh) — e.g. an org that
  /// just enabled OAuth access, or a machine that just came online.
  Future<void> _refresh() {
    switch (_level) {
      case _Level.orgs:
        return _loadOrgs();
      case _Level.locations:
        return _loadLocations(_selectedOrg);
      case _Level.robots:
        return _loadRobots(_selectedLoc);
    }
  }

  /// One level up: machines → locations → organizations.
  void _goBack() {
    switch (_level) {
      case _Level.robots:
        _loadLocations(_selectedOrg);
      case _Level.locations:
        _loadOrgs();
      case _Level.orgs:
        break; // top level — no back button shown
    }
  }

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
      await _rememberMachine(
        robotId: robot.id.toString(),
        orgId: _orgId,
        name: robot.name?.toString() ?? '',
      );
      widget.onConnected(client, robot.id.toString());
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

  /// Dial via [MachineConnector] — the shared cloud/API-key connect path,
  /// also used for launch auto-connect and dead-connection re-dials.
  Future<RobotClient> _connectRobot(String robotId, {String? cachedFqdn}) async {
    // Dialing needs app.viam.com (to resolve the address and verify or mint
    // the machine key), so the session has to be current first.
    await widget.session.validAccessToken();
    final viam = widget.session.viam;
    if (viam == null) throw StateError('signed out');
    final connector = MachineConnector(viam: viam);
    final client =
        await connector.connect(robotId, _orgId, cachedFqdn: cachedFqdn);
    _lastFqdn = connector.fqdn;
    return client;
  }

  /// Store the machine to reconnect to, including the address the dial
  /// resolved — a re-dial that skips the app.viam.com lookup recovers
  /// noticeably faster, and works on a link too marginal for the extra calls.
  Future<void> _rememberMachine({
    required String robotId,
    required String orgId,
    required String name,
  }) async {
    try {
      await _storage.write(
        key: kLastMachineKey,
        value: jsonEncode({
          'robotId': robotId,
          'orgId': orgId,
          'name': name,
          if (_lastFqdn != null) 'fqdn': _lastFqdn,
        }),
      );
    } catch (_) {
      // Storage unavailable — auto-connect just won't survive a restart.
    }
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
    final warning = widget.session.warning;
    return Scaffold(
      appBar: AppBar(
        // Back one level (machines → locations → orgs) at every level below
        // the top; without this the only way out of a wrong tap was signing
        // out and starting over.
        leading: (_level != _Level.orgs && !_connecting)
            ? IconButton(
                tooltip: 'Back',
                icon: const Icon(Icons.arrow_back),
                onPressed: _goBack,
              )
            : null,
        title: Text(_title),
        actions: [
          IconButton(
            tooltip: 'Sign out',
            onPressed: widget.session.signOut,
            icon: const Icon(Icons.logout),
          ),
        ],
      ),
      body: Column(
        children: [
          if (warning != null) _SessionWarning(message: warning),
          Expanded(child: _body()),
        ],
      ),
      // Who this session belongs to — the answer to "why can't I see my
      // org" is usually "wrong account".
      bottomNavigationBar: SafeArea(
        child: Padding(
          padding: const EdgeInsets.symmetric(vertical: 8),
          child: Text(
            'Signed in as ${widget.session.userEmail ?? 'unknown user'}',
            textAlign: TextAlign.center,
            style: Theme.of(context)
                .textTheme
                .bodySmall
                ?.copyWith(color: Colors.white54),
          ),
        ),
      ),
    );
  }

  Widget _body() {
    if (_connecting) {
      return Center(
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
      );
    }
    if (_loading) return const Center(child: CircularProgressIndicator());
    if (_error != null) {
      return _ErrorRetry(message: _error!, onRetry: _loadOrgs);
    }
    final hint = switch (_level) {
      _Level.orgs => 'Search organizations…',
      _Level.locations => 'Search locations…',
      _Level.robots => 'Search machines…',
    };
    return Column(
      children: [
        // Per-page search: filters the list that's showing, nothing more.
        Padding(
          padding: const EdgeInsets.fromLTRB(16, 8, 16, 4),
          child: TextField(
            controller: _searchCtl,
            decoration: InputDecoration(
              hintText: hint,
              prefixIcon: const Icon(Icons.search),
              isDense: true,
              border:
                  OutlineInputBorder(borderRadius: BorderRadius.circular(10)),
              suffixIcon: _query.isEmpty
                  ? null
                  : IconButton(
                      icon: const Icon(Icons.clear),
                      onPressed: () {
                        _searchCtl.clear();
                        setState(() => _query = '');
                      },
                    ),
            ),
            onChanged: (v) => setState(() => _query = v),
          ),
        ),
        Expanded(child: _levelList()),
      ],
    );
  }

  Widget _levelList() {
    final shown = filterByName(_items, _query);
    if (shown.isEmpty && _query.trim().isNotEmpty) {
      return const Center(child: Text('No matches'));
    }
    return RefreshIndicator(
      onRefresh: _refresh,
      child: ListView.separated(
        // Scrollable even when short, so pull-to-refresh works
        // on a two-row list.
        physics: const AlwaysScrollableScrollPhysics(),
        itemCount: shown.length,
        separatorBuilder: (_, __) => const Divider(height: 1),
        itemBuilder: (context, i) {
          final item = shown[i];
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

/// Banner for a session that is signed in but won't survive — the two causes
/// (no refresh token, unusable secure storage) both present to the user as
/// "it made me log in again", so naming which one it is matters.
class _SessionWarning extends StatelessWidget {
  const _SessionWarning({required this.message});
  final String message;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Container(
      width: double.infinity,
      color: scheme.errorContainer,
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(Icons.warning_amber, size: 18, color: scheme.onErrorContainer),
          const SizedBox(width: 8),
          Expanded(
            child: SelectableText(
              message,
              style: TextStyle(fontSize: 12, color: scheme.onErrorContainer),
            ),
          ),
        ],
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
