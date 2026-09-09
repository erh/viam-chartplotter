import 'package:flutter/material.dart';
import 'package:latlong2/latlong.dart';

import '../app_config.dart';
import '../chart/chart_search.dart';

/// What the user chose from the search sheet: go look at [hit], or (when
/// [route] is set) plan an auto-route from the boat to it.
typedef SearchChoice = ({SearchHit hit, bool route});

/// Chart search bottom sheet: a type-ahead over the ENC feature store —
/// lights, wrecks, canyons, channels, anchorages, harbours. The web puts
/// this on the map (marineMap.svelte) because the answer to "where is X" is
/// a place; here a full-height sheet keeps the keyboard clear of the list.
/// Pops with a [SearchChoice], or null when dismissed.
class ChartSearchSheet extends StatefulWidget {
  const ChartSearchSheet({super.key, this.origin, this.canRoute = false});

  /// Where to rank distances from (the map centre, web parity); null ranks
  /// alphabetically.
  final LatLng? origin;

  /// Show the per-hit "route here" action (needs a nav service and a fix).
  final bool canRoute;

  @override
  State<ChartSearchSheet> createState() => _ChartSearchSheetState();
}

class _ChartSearchSheetState extends State<ChartSearchSheet> {
  late final SearchRunner<SearchResponse> _runner = SearchRunner(
    (q) => searchChart(AppConfig.tileBase.value, q,
        origin: widget.origin, limit: 12),
    const SearchResponse(hits: [], matchedQuery: ''),
  );

  String _term = '';
  List<SearchHit> _hits = const [];
  // Set when the server matched fewer words than were typed (see searchChart):
  // the results then answer a narrower question than was asked, and the
  // operator needs to know that before steering at one.
  String _matched = '';
  bool _busy = false;
  String? _error;

  @override
  void dispose() {
    _runner.cancel();
    super.dispose();
  }

  void _onChanged(String q) {
    setState(() {
      _term = q;
      _error = null;
      _busy = q.trim().length >= minQueryLength;
    });
    _runner.search(
      q,
      (res, sent) {
        if (!mounted) return;
        setState(() {
          _hits = res.hits;
          _matched = res.matchedQuery.isNotEmpty &&
                  res.matchedQuery != sent.trim()
              ? res.matchedQuery
              : '';
          _busy = false;
        });
      },
      (e) {
        if (!mounted) return;
        setState(() {
          _error = e is Exception
              ? e.toString().replaceFirst('Exception: ', '')
              : '$e';
          _hits = const [];
          _matched = '';
          _busy = false;
        });
      },
    );
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      // Ride above the keyboard so the field and results stay visible.
      padding: EdgeInsets.only(
          bottom: MediaQuery.of(context).viewInsets.bottom),
      child: SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 0, 16, 12),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              TextField(
                autofocus: true,
                textInputAction: TextInputAction.search,
                decoration: InputDecoration(
                  prefixIcon: const Icon(Icons.search),
                  suffixIcon: _busy
                      ? const Padding(
                          padding: EdgeInsets.all(12),
                          child: SizedBox(
                              width: 18,
                              height: 18,
                              child:
                                  CircularProgressIndicator(strokeWidth: 2)),
                        )
                      : null,
                  hintText: 'Lights, wrecks, channels, harbors…',
                ),
                onChanged: _onChanged,
              ),
              const SizedBox(height: 8),
              if (_error case final err?)
                Padding(
                  padding: const EdgeInsets.symmetric(vertical: 8),
                  child: Text(err,
                      style: const TextStyle(color: Colors.redAccent)),
                )
              else if (_matched.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.symmetric(vertical: 4),
                  child: Text('No full match — showing “$_matched”',
                      style: const TextStyle(color: Colors.orangeAccent)),
                )
              else if (_hits.isEmpty &&
                  !_busy &&
                  _term.trim().length >= minQueryLength)
                const Padding(
                  padding: EdgeInsets.symmetric(vertical: 8),
                  child: Text('Nothing named that on the chart'),
                ),
              Flexible(
                child: ListView(
                  shrinkWrap: true,
                  children: [
                    for (final h in _hits)
                      ListTile(
                        contentPadding: EdgeInsets.zero,
                        title: Text(h.name,
                            maxLines: 1, overflow: TextOverflow.ellipsis),
                        subtitle: Text(
                          [
                            h.label,
                            if (h.address.isNotEmpty) h.address,
                            if (h.area.isNotEmpty) h.area,
                            if (formatSearchDistance(h.distanceMeters)
                                case final d when d.isNotEmpty)
                              d,
                          ].join(' · '),
                        ),
                        trailing: widget.canRoute
                            ? IconButton(
                                tooltip: 'Auto route here',
                                icon: const Icon(Icons.alt_route),
                                onPressed: () => Navigator.pop(
                                    context, (hit: h, route: true)),
                              )
                            : null,
                        onTap: () =>
                            Navigator.pop(context, (hit: h, route: false)),
                      ),
                  ],
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
