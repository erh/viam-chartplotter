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

/// One machine found by the cross-org boat search: the robot plus where it
/// lives, so a result can both label itself and connect directly.
typedef BoatHit = ({dynamic robot, dynamic org, dynamic loc});

/// Case-insensitive boat filter for the org-screen search: matches the
/// machine or LOCATION name (people often name the location after the
/// boat). Deliberately not the org name — that matched every boat in the
/// org at once, which is noise, not search. Dedupes by machine id (a
/// shared location is reachable through two orgs), sorts by machine name.
/// Pure, for tests.
List<BoatHit> filterBoatHits(List<BoatHit> all, String query) {
  final q = query.trim().toLowerCase();
  if (q.isEmpty) return const [];
  bool has(dynamic v) => (v?.toString().toLowerCase() ?? '').contains(q);
  final seen = <String>{};
  final out = [
    for (final h in all)
      if ((has(h.robot.name) || has(h.loc.name)) &&
          seen.add(h.robot.id?.toString() ??
              identityHashCode(h.robot).toString()))
        h
  ];
  out.sort((a, b) => (a.robot.name?.toString().toLowerCase() ?? '')
      .compareTo(b.robot.name?.toString().toLowerCase() ?? ''));
  return out;
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

  // ---- boat search (org screen) ----------------------------------------
  // Typing in the search box kicks off a one-time background walk of every
  // org → location → machine; hits stream into the list as each location
  // answers, so a match in the first org shows while later orgs still load.
  // The index lives for the screen's lifetime — clearing and re-searching
  // is instant.
  final TextEditingController _searchCtl = TextEditingController();
  String _query = '';
  final List<BoatHit> _allBoats = [];
  bool _indexStarted = false;
  bool _indexing = false;
  String _indexProgress = '';

  Future<void> _buildBoatIndex() async {
    if (_indexStarted) return;
    _indexStarted = true;
    _indexing = true;
    try {
      await widget.session.validAccessToken();
      final orgs = _byName(await _viam.appClient.listOrganizations());
      for (var i = 0; i < orgs.length; i++) {
        if (!mounted) return;
        final org = orgs[i];
        setState(() => _indexProgress =
            'searching ${org.name} (${i + 1}/${orgs.length})…');
        try {
          final locs = await _viam.appClient.listLocations(org.id);
          for (final loc in locs) {
            final robots = await _viam.appClient.listRobots(loc.id);
            if (!mounted) return;
            setState(() {
              for (final r in robots) {
                _allBoats.add((robot: r, org: org, loc: loc));
              }
            });
          }
        } catch (_) {
          // An org we can list but not read into — skip it, keep searching.
        }
      }
    } catch (e) {
      // Total failure (offline): allow a retry on the next keystroke.
      _indexStarted = false;
      if (mounted) setState(() => _error = 'Search failed: $e');
    } finally {
      _indexing = false;
      if (mounted) setState(() => _indexProgress = '');
    }
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
        });
      });

  Future<void> _loadRobots(dynamic loc) => _guard(() async {
        _selectedLoc = loc;
        final robots = await _viam.appClient.listRobots(loc.id);
        setState(() {
          _level = _Level.robots;
          _title = 'Select machine';
          _items = _byName(robots);
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
    final searching = _level == _Level.orgs && _query.trim().isNotEmpty;
    return Column(
      children: [
        // Boat search, on the top (org) screen only: finds a machine by
        // name across every org/location without walking the tree.
        if (_level == _Level.orgs)
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 8, 16, 4),
            child: TextField(
              controller: _searchCtl,
              decoration: InputDecoration(
                hintText: 'Search boats…',
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
              onChanged: (v) {
                setState(() => _query = v);
                // First keystroke starts the one-time index build.
                if (v.trim().isNotEmpty) _buildBoatIndex();
              },
            ),
          ),
        Expanded(child: searching ? _searchResults() : _levelList()),
      ],
    );
  }

  Widget _levelList() {
    return RefreshIndicator(
      onRefresh: _refresh,
      child: ListView.separated(
        // Scrollable even when short, so pull-to-refresh works
        // on a two-row list.
        physics: const AlwaysScrollableScrollPhysics(),
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

  /// Search hits, streaming in while the index walk is still running. A hit
  /// carries its org, so tapping connects directly — no tree walk.
  Widget _searchResults() {
    final hits = filterBoatHits(_allBoats, _query);
    return ListView(
      children: [
        for (final h in hits)
          ListTile(
            leading: const Icon(Icons.sailing),
            title: Text(h.robot.name?.toString() ?? '(unnamed)'),
            subtitle: Text('${h.org.name ?? ''} · ${h.loc.name ?? ''}'),
            trailing: const Icon(Icons.link),
            onTap: () {
              _orgId = h.org.id;
              _selectedOrg = h.org;
              _selectedLoc = h.loc;
              _connect(h.robot);
            },
          ),
        if (_indexing)
          Padding(
            padding: const EdgeInsets.all(16),
            child: Row(
              children: [
                const SizedBox(
                    width: 16,
                    height: 16,
                    child: CircularProgressIndicator(strokeWidth: 2)),
                const SizedBox(width: 12),
                Expanded(
                  child: Text(_indexProgress,
                      style: Theme.of(context).textTheme.bodySmall),
                ),
              ],
            ),
          )
        else if (hits.isEmpty)
          const Padding(
            padding: EdgeInsets.all(24),
            child: Center(child: Text('No boats match')),
          ),
      ],
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
