import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../ais.dart';

/// Viewport cull + cap for the AIS layer (D10).
///
/// Every target used to become a rotated marker widget on every 1 Hz state
/// tick — hundreds of `Transform.rotate`s per second in a busy harbour. The
/// wind overlay already learned this lesson (cached markers, event-driven
/// rebuilds, a cap); this is AIS's version, kept pure so it's unit-testable.
///
/// [bounds] is the visible viewport, expanded by [marginFraction] per side so
/// panning doesn't pop targets at the edge. Over [cap], targets closest to
/// [reference] (the view center) win — a silently truncated traffic picture
/// is worse than a visibly capped one, so the counts are reported back for
/// the debug screen.
({List<AisBoat> shown, int culled, int capped}) cullAisTargets({
  required List<AisBoat> boats,
  LatLngBounds? bounds,
  LatLng? reference,
  int cap = 500,
  double marginFraction = 0.3,
}) {
  var candidates = boats;
  var culled = 0;
  if (bounds != null) {
    final latMargin = (bounds.north - bounds.south) * marginFraction;
    final lonMargin = (bounds.east - bounds.west) * marginFraction;
    final south = bounds.south - latMargin;
    final north = bounds.north + latMargin;
    final west = bounds.west - lonMargin;
    final east = bounds.east + lonMargin;
    candidates = [
      for (final b in boats)
        if (b.location.latitude >= south &&
            b.location.latitude <= north &&
            b.location.longitude >= west &&
            b.location.longitude <= east)
          b
    ];
    culled = boats.length - candidates.length;
  }
  var capped = 0;
  if (candidates.length > cap) {
    if (reference != null) {
      const d = Distance();
      final sorted = List.of(candidates)
        ..sort((a, b) => d
            .distance(reference, a.location)
            .compareTo(d.distance(reference, b.location)));
      candidates = sorted;
    }
    capped = candidates.length - cap;
    candidates = candidates.sublist(0, cap);
  }
  return (shown: candidates, culled: culled, capped: capped);
}
