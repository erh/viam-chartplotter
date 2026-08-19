import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter_map/flutter_map.dart';

/// Viewport-driven GeoJSON feature source (B4) — flutter_map's stand-in for
/// OpenLayers' bboxStrategy + capVectorSource. B1/B2 are unusable without
/// it: naive per-pan refetching hammers the server, and naive accumulation
/// grows without bound over a long coastal session.
///
/// - Extents already loaded are never refetched (panning back over loaded
///   water issues no request).
/// - Viewport changes are debounced to the pan/zoom settling, not per frame.
/// - Requests coalesce: one in flight, at most one pending behind it.
/// - Past [cap] retained features (web caps at 3000 per layer), everything
///   is evicted and the current viewport reloads.
class BboxFeatureSource<T> extends ChangeNotifier {
  BboxFeatureSource({
    required this.fetch,
    this.cap = 3000,
    this.debounce = const Duration(milliseconds: 400),
    this.padFraction = 0.25,
  });

  /// Fetches features for a bbox (west, south, east, north).
  final Future<List<T>> Function(
      double west, double south, double east, double north) fetch;
  final int cap;
  final Duration debounce;

  /// Extents are padded so small pans stay inside already-loaded water.
  final double padFraction;

  final List<T> features = [];
  final List<(double, double, double, double)> _loaded = [];
  Timer? _debounce;
  bool _inFlight = false;
  LatLngBounds? _pending;

  /// Call from onPositionChanged with the settled viewport.
  void viewportChanged(LatLngBounds bounds) {
    _debounce?.cancel();
    _debounce = Timer(debounce, () => _load(bounds));
  }

  Future<void> _load(LatLngBounds bounds) async {
    if (_inFlight) {
      _pending = bounds; // coalesce: newest wins
      return;
    }
    // Coverage is tested on the RAW viewport; the fetch (and the recorded
    // extent) are padded — that's what makes small pans free.
    final raw = (bounds.west, bounds.south, bounds.east, bounds.north);
    if (extentCovered(_loaded, raw)) return;
    final want = _pad(bounds);
    _inFlight = true;
    try {
      final got = await fetch(want.$1, want.$2, want.$3, want.$4);
      _loaded.add(want);
      features.addAll(got);
      if (features.length > cap) {
        // Web's capVectorSource: drop everything, repopulate the viewport.
        features.clear();
        _loaded.clear();
        final again = await fetch(want.$1, want.$2, want.$3, want.$4);
        _loaded.add(want);
        features.addAll(again);
      }
      notifyListeners();
    } catch (_) {
      // Server unreachable — leave what we have; a later pan retries.
    } finally {
      _inFlight = false;
      final p = _pending;
      _pending = null;
      if (p != null) unawaited(_load(p));
    }
  }

  (double, double, double, double) _pad(LatLngBounds b) {
    final latPad = (b.north - b.south) * padFraction;
    final lonPad = (b.east - b.west) * padFraction;
    return (
      b.west - lonPad,
      b.south - latPad,
      b.east + lonPad,
      b.north + latPad,
    );
  }

  @override
  void dispose() {
    _debounce?.cancel();
    super.dispose();
  }
}

/// True when [want] (w, s, e, n) lies entirely inside any one loaded extent.
/// Deliberately single-extent containment, not a union test — unioning
/// rectangles correctly is where the bugs live, and a rare duplicate fetch
/// is cheaper than a wrong "covered".
bool extentCovered(
  List<(double, double, double, double)> loaded,
  (double, double, double, double) want,
) {
  for (final l in loaded) {
    if (want.$1 >= l.$1 && want.$2 >= l.$2 && want.$3 <= l.$3 && want.$4 <= l.$4) {
      return true;
    }
  }
  return false;
}
