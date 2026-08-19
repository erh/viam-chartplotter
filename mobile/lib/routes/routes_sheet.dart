import 'dart:io';

// Flutter has its own navigator `Route`; ours (route_store) wins here.
import 'package:flutter/material.dart' hide Route;
import 'package:latlong2/latlong.dart';
import 'package:share_plus/share_plus.dart';

import '../boat_state.dart';
import '../chart/areas.dart' show parseCssColor;
import '../gpx.dart';
import '../simplify.dart';
import 'route_store.dart';

/// A route preview to draw on the chart (display-only, dashed).
typedef RoutePreview = ({List<LatLng> points, Color color});

/// Saved-routes bottom sheet (E2): list, load, load-reversed, rename, delete,
/// colour pick, preview-on-map. The mobile RoutesPanel. All persistence goes
/// through the nav service's routes_* DoCommands via [api]; previews are
/// pushed up to MapScreen through [onPreviews] and stay on the chart after
/// the sheet closes (unlike web's sidebar, the sheet covers the map — a
/// preview cleared on close would never be seen).
class RoutesSheet extends StatefulWidget {
  const RoutesSheet({
    super.key,
    required this.state,
    required this.api,
    required this.onLoad,
    required this.onPreviews,
    this.previewedIds = const {},
    this.showAll = false,
    this.fetchTrack,
    this.onTrackPreview,
  });

  final BoatState state;
  final RoutesApi api;

  /// Replace the active route with these waypoints (one atomic
  /// set_waypoints). Throws on failure so the sheet can show why.
  final Future<void> Function(List<LatLng> waypoints) onLoad;

  /// Push the current preview set (and the toggles that produced it, so a
  /// reopened sheet restores them) up to the map.
  final void Function(
      Map<String, RoutePreview> previews, Set<String> ids, bool showAll)
      onPreviews;

  /// Previously-previewed route ids / show-all, restored on reopen.
  final Set<String> previewedIds;
  final bool showAll;

  /// Recorded track for an explicit window (E3 save-from-track), from the
  /// cloud data store. Null (API-key path / no movement sensor) hides the
  /// feature.
  final Future<List<LatLng>> Function(DateTime t0, DateTime t1)? fetchTrack;

  /// Show the simplified candidate route on the chart (null clears it).
  final void Function(List<LatLng>? points)? onTrackPreview;

  @override
  State<RoutesSheet> createState() => _RoutesSheetState();
}

class _RoutesSheetState extends State<RoutesSheet> {
  List<Route> _routes = const [];
  bool _loading = true;
  bool _busy = false;
  String? _error;
  late final Set<String> _previewIds = {...widget.previewedIds};
  late bool _showAll = widget.showAll;

  @override
  void initState() {
    super.initState();
    _refresh();
  }

  Future<void> _refresh() async {
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      final routes = await listRoutes(widget.api);
      if (!mounted) return;
      setState(() {
        _routes = routes;
        _loading = false;
      });
      _pushPreviews();
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _error = '$e';
        _loading = false;
      });
    }
  }

  /// Run one mutation, then re-list (web's withStore).
  Future<void> _withStore(Future<void> Function(RoutesApi api) fn) async {
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      await fn(widget.api);
      final routes = await listRoutes(widget.api);
      if (!mounted) return;
      setState(() => _routes = routes);
      _pushPreviews();
    } catch (e) {
      if (!mounted) return;
      setState(() => _error = '$e');
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  void _pushPreviews() {
    final previews = <String, RoutePreview>{};
    for (final r in _routes) {
      if ((_showAll || _previewIds.contains(r.id)) && r.waypoints.length >= 2) {
        previews[r.id] = (
          points: r.waypoints,
          color: parseCssColor(r.color, fallback: Colors.orangeAccent),
        );
      }
    }
    widget.onPreviews(previews, {..._previewIds}, _showAll);
  }

  Future<void> _load(Route r, {bool reversed = false}) async {
    final wps = reversed ? r.waypoints.reversed.toList() : r.waypoints;
    final current =
        widget.state.navWaypoints.where((w) => !w.isPending).length;
    if (current > 0) {
      final ok = await showDialog<bool>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: const Text('Replace active route?'),
          content: Text(
              'Replace the current $current waypoint(s) with "${r.name}"'
              '${reversed ? ' (reversed)' : ''}?'),
          actions: [
            TextButton(
                onPressed: () => Navigator.pop(ctx, false),
                child: const Text('Cancel')),
            FilledButton(
                onPressed: () => Navigator.pop(ctx, true),
                child: const Text('Replace')),
          ],
        ),
      );
      if (ok != true || !mounted) return;
    }
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      await widget.onLoad(wps);
      if (mounted) Navigator.of(context).pop(); // show the map, route is live
    } catch (e) {
      if (mounted) {
        setState(() {
          _error = 'Load failed: $e';
          _busy = false;
        });
      }
    }
  }

  Future<void> _rename(Route r) async {
    final nameCtl = TextEditingController(text: r.name);
    final notesCtl = TextEditingController(text: r.notes ?? '');
    var color = r.color ?? routePalette.first;
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => StatefulBuilder(
        builder: (ctx, setDialogState) => AlertDialog(
          title: const Text('Edit route'),
          content: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              TextField(
                controller: nameCtl,
                decoration: const InputDecoration(labelText: 'Name'),
              ),
              TextField(
                controller: notesCtl,
                decoration: const InputDecoration(labelText: 'Notes'),
              ),
              const SizedBox(height: 12),
              _PalettePicker(
                selected: color,
                onPick: (c) => setDialogState(() => color = c),
              ),
            ],
          ),
          actions: [
            TextButton(
                onPressed: () => Navigator.pop(ctx, false),
                child: const Text('Cancel')),
            FilledButton(
                onPressed: () => Navigator.pop(ctx, true),
                child: const Text('Save')),
          ],
        ),
      ),
    );
    if (ok != true) return;
    final name = nameCtl.text.trim();
    await _withStore((api) => renameRoute(
          api,
          r.id,
          name: name.isEmpty ? null : name,
          notes: notesCtl.text.trim(),
          color: color,
          nowIso: DateTime.now().toUtc().toIso8601String(),
        ));
  }

  Future<void> _delete(Route r) async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text('Delete "${r.name}"?'),
        content: const Text("This can't be undone."),
        actions: [
          TextButton(
              onPressed: () => Navigator.pop(ctx, false),
              child: const Text('Cancel')),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: Colors.red),
            onPressed: () => Navigator.pop(ctx, true),
            child: const Text('Delete'),
          ),
        ],
      ),
    );
    if (ok != true) return;
    _previewIds.remove(r.id);
    await _withStore((api) => deleteRoute(api, r.id));
  }

  /// Save the active waypoints as a new route (web's "Save current
  /// waypoints…"). Pending ids are fine here — only positions are saved.
  Future<void> _saveCurrent() async {
    final wps = [for (final w in widget.state.navWaypoints) w.pos];
    final nameCtl = TextEditingController();
    var color = nextColor(_routes);
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => StatefulBuilder(
        builder: (ctx, setDialogState) => AlertDialog(
          title: Text('Save ${wps.length} waypoints as route'),
          content: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              TextField(
                controller: nameCtl,
                autofocus: true,
                decoration: const InputDecoration(labelText: 'Name'),
              ),
              const SizedBox(height: 12),
              _PalettePicker(
                selected: color,
                onPick: (c) => setDialogState(() => color = c),
              ),
            ],
          ),
          actions: [
            TextButton(
                onPressed: () => Navigator.pop(ctx, false),
                child: const Text('Cancel')),
            FilledButton(
                onPressed: () => Navigator.pop(ctx, true),
                child: const Text('Save')),
          ],
        ),
      ),
    );
    final name = nameCtl.text.trim();
    if (ok != true || name.isEmpty) return;
    final now = DateTime.now().toUtc().toIso8601String();
    await _withStore((api) => saveRoute(
          api,
          Route(
            id: newRouteId(),
            name: name,
            color: color,
            source: 'manual',
            createdAt: now,
            updatedAt: now,
            waypoints: wps,
          ),
        ));
  }

  /// GPX export (E4): write the route to a temp file and hand it to the
  /// platform share sheet — the mobile equivalent of the web download, and
  /// how a route gets onto a Garmin (Garmin/GPX/*.gpx on a card).
  Future<void> _exportGpx(Route r) async {
    try {
      final file = File('${Directory.systemTemp.path}/${gpxFilename(r.name)}');
      await file.writeAsString(routeToGpx(r.name, r.waypoints));
      await Share.shareXFiles(
        [XFile(file.path, mimeType: 'application/gpx+xml')],
        subject: r.name,
      );
    } catch (e) {
      if (mounted) setState(() => _error = 'GPX export failed: $e');
    }
  }

  /// Save-from-track (E3): pick a window ending now, pull the recorded
  /// track, simplify (Douglas–Peucker via simplifyTrack), preview on the
  /// chart, save with source "track". The preview IS the saved geometry —
  /// both come from the same simplified list.
  Future<void> _saveFromTrack() async {
    final fetch = widget.fetchTrack;
    if (fetch == null) return;
    final nameCtl = TextEditingController();
    var hours = 4;
    var granularityM = 50;
    List<LatLng>? raw; // fetched window, cached across granularity changes
    SimplifiedTrack? simplified;
    var fetching = false;
    String? err;

    SimplifiedTrack? resimplify() => raw == null || raw!.isEmpty
        ? null
        : simplifyTrack(
            raw!,
            SimplifyOptions(
                granularityMeters: granularityM.toDouble(), maxPoints: 200));

    final saved = await showDialog<bool>(
      context: context,
      builder: (ctx) => StatefulBuilder(
        builder: (ctx, setDialogState) {
          Future<void> doFetch() async {
            setDialogState(() {
              fetching = true;
              err = null;
            });
            try {
              final now = DateTime.now();
              raw = await fetch(now.subtract(Duration(hours: hours)), now);
              simplified = resimplify();
              widget.onTrackPreview?.call(simplified?.waypoints);
              if (raw!.isEmpty) err = 'No recorded track in that window.';
            } catch (e) {
              err = 'Track fetch failed: $e';
            }
            setDialogState(() => fetching = false);
          }

          final s = simplified;
          return AlertDialog(
            title: const Text('Route from track'),
            content: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Row(
                  children: [
                    const Expanded(child: Text('Window')),
                    DropdownButton<int>(
                      value: hours,
                      items: [
                        for (final h in const [1, 2, 4, 8, 12, 24, 48])
                          DropdownMenuItem(value: h, child: Text('last ${h}h')),
                      ],
                      onChanged: (v) =>
                          setDialogState(() => hours = v ?? hours),
                    ),
                  ],
                ),
                Row(
                  children: [
                    const Expanded(child: Text('Granularity')),
                    DropdownButton<int>(
                      value: granularityM,
                      items: [
                        for (final g in const [25, 50, 100, 250, 500])
                          DropdownMenuItem(value: g, child: Text('$g m')),
                      ],
                      onChanged: (v) {
                        setDialogState(() {
                          granularityM = v ?? granularityM;
                          simplified = resimplify();
                        });
                        widget.onTrackPreview?.call(simplified?.waypoints);
                      },
                    ),
                  ],
                ),
                TextButton.icon(
                  onPressed: fetching ? null : doFetch,
                  icon: fetching
                      ? const SizedBox(
                          width: 14,
                          height: 14,
                          child: CircularProgressIndicator(strokeWidth: 2))
                      : const Icon(Icons.download),
                  label: const Text('Fetch & preview'),
                ),
                if (err != null)
                  Text(err!, style: const TextStyle(color: Colors.redAccent)),
                if (s != null && s.waypoints.isNotEmpty) ...[
                  Text('${s.inputCount} fixes → ${s.waypoints.length} '
                      'waypoints · '
                      '${(pathLengthMeters(s.waypoints) / 1852).toStringAsFixed(1)} nm'),
                  if (s.capped)
                    const Text(
                      'Hit the waypoint cap — raise granularity for detail.',
                      style: TextStyle(color: Colors.orangeAccent),
                    ),
                ],
                TextField(
                  controller: nameCtl,
                  decoration: const InputDecoration(labelText: 'Route name'),
                ),
              ],
            ),
            actions: [
              TextButton(
                  onPressed: () => Navigator.pop(ctx, false),
                  child: const Text('Cancel')),
              FilledButton(
                onPressed: (s != null &&
                        s.waypoints.length >= 2 &&
                        !fetching)
                    ? () => Navigator.pop(ctx, true)
                    : null,
                child: const Text('Save route'),
              ),
            ],
          );
        },
      ),
    );

    final s = simplified;
    final name = nameCtl.text.trim();
    if (saved == true && s != null && s.waypoints.length >= 2) {
      final now = DateTime.now().toUtc().toIso8601String();
      await _withStore((api) => saveRoute(
            api,
            Route(
              id: newRouteId(),
              name: name.isEmpty ? 'Track route' : name,
              color: nextColor(_routes),
              source: 'track',
              createdAt: now,
              updatedAt: now,
              waypoints: s.waypoints,
            ),
          ));
    }
    widget.onTrackPreview?.call(null); // candidate gone; the list previews it
  }

  String _subtitle(Route r) {
    final n = r.count ?? r.waypoints.length;
    final parts = [
      '$n wp',
      if (r.distanceNm != null) '${r.distanceNm!.toStringAsFixed(1)} nm',
      if (r.source == 'track') 'from track',
      if (r.readOnly) 'read-only',
    ];
    return parts.join(' · ');
  }

  @override
  Widget build(BuildContext context) {
    final activeWps = widget.state.navWaypoints;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 0, 16, 16),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Text('Routes', style: Theme.of(context).textTheme.titleLarge),
                const Spacer(),
                if (_busy)
                  const SizedBox(
                      width: 18,
                      height: 18,
                      child: CircularProgressIndicator(strokeWidth: 2)),
                TextButton.icon(
                  onPressed: () {
                    setState(() => _showAll = !_showAll);
                    _pushPreviews();
                  },
                  icon: Icon(
                      _showAll ? Icons.visibility : Icons.visibility_outlined),
                  label: const Text('Show all'),
                ),
              ],
            ),
            if (_error != null)
              Padding(
                padding: const EdgeInsets.only(bottom: 8),
                child: Text(_error!,
                    style: const TextStyle(color: Colors.redAccent)),
              ),
            if (sizeWarning(_routes))
              const Padding(
                padding: EdgeInsets.only(bottom: 8),
                child: Text(
                  'Routes are near the storage size limit — delete some '
                  'before saving more.',
                  style: TextStyle(color: Colors.orangeAccent),
                ),
              ),
            if (_loading)
              const Padding(
                padding: EdgeInsets.all(24),
                child: Center(child: CircularProgressIndicator()),
              )
            else if (_routes.isEmpty)
              const Padding(
                padding: EdgeInsets.all(16),
                child: Text('No saved routes yet.'),
              )
            else
              Flexible(
                child: ListView(
                  shrinkWrap: true,
                  children: [
                    for (final r in _routes)
                      ListTile(
                        contentPadding: EdgeInsets.zero,
                        leading: Icon(Icons.circle,
                            size: 16,
                            color: parseCssColor(r.color,
                                fallback: Colors.orangeAccent)),
                        title: Row(
                          children: [
                            Flexible(
                                child: Text(r.name,
                                    overflow: TextOverflow.ellipsis)),
                            if (r.readOnly)
                              const Padding(
                                padding: EdgeInsets.only(left: 6),
                                child: Icon(Icons.lock, size: 14),
                              ),
                          ],
                        ),
                        subtitle: Text(_subtitle(r)),
                        onTap: () {
                          setState(() {
                            _previewIds.contains(r.id)
                                ? _previewIds.remove(r.id)
                                : _previewIds.add(r.id);
                          });
                          _pushPreviews();
                        },
                        selected: _showAll || _previewIds.contains(r.id),
                        trailing: PopupMenuButton<String>(
                          enabled: !_busy,
                          onSelected: (v) {
                            switch (v) {
                              case 'load':
                                _load(r);
                              case 'load-rev':
                                _load(r, reversed: true);
                              case 'gpx':
                                _exportGpx(r);
                              case 'edit':
                                _rename(r);
                              case 'delete':
                                _delete(r);
                            }
                          },
                          itemBuilder: (_) => [
                            const PopupMenuItem(
                                value: 'load', child: Text('Load')),
                            const PopupMenuItem(
                                value: 'load-rev',
                                child: Text('Load reversed')),
                            const PopupMenuItem(
                                value: 'gpx', child: Text('Export GPX…')),
                            // Inherited (parent-scope) routes are read-only:
                            // no rename/recolour/delete offered at all.
                            if (!r.readOnly) ...[
                              const PopupMenuItem(
                                  value: 'edit', child: Text('Rename…')),
                              const PopupMenuItem(
                                  value: 'delete', child: Text('Delete')),
                            ],
                          ],
                        ),
                      ),
                  ],
                ),
              ),
            const SizedBox(height: 4),
            Row(
              children: [
                if (activeWps.isNotEmpty)
                  TextButton.icon(
                    onPressed: _busy ? null : _saveCurrent,
                    icon: const Icon(Icons.save_alt),
                    label: Text('Save current (${activeWps.length})'),
                  ),
                if (widget.fetchTrack != null)
                  TextButton.icon(
                    onPressed: _busy ? null : _saveFromTrack,
                    icon: const Icon(Icons.timeline),
                    label: const Text('From track'),
                  ),
                const Spacer(),
                TextButton.icon(
                  onPressed: _busy ? null : _refresh,
                  icon: const Icon(Icons.refresh),
                  label: const Text('Refresh'),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}

/// Row of tappable palette dots (colour pick for save/rename).
class _PalettePicker extends StatelessWidget {
  const _PalettePicker({required this.selected, required this.onPick});
  final String selected;
  final void Function(String) onPick;

  @override
  Widget build(BuildContext context) {
    return Wrap(
      spacing: 8,
      children: [
        for (final c in routePalette)
          GestureDetector(
            onTap: () => onPick(c),
            child: Container(
              width: 28,
              height: 28,
              decoration: BoxDecoration(
                shape: BoxShape.circle,
                color: parseCssColor(c),
                border: Border.all(
                  color: c == selected ? Colors.white : Colors.black26,
                  width: c == selected ? 3 : 1,
                ),
              ),
            ),
          ),
      ],
    );
  }
}
