import 'package:flutter/material.dart';
import 'package:latlong2/latlong.dart';

/// Live own-boat track (C2): where you've just been, which is how you spot
/// set and drift. Points accumulate per poll tick when the boat has actually
/// moved; segments carry the depth at the time so the track can be coloured
/// by depth (shoal transits read red).

/// One recorded track point.
class TrackPoint {
  const TrackPoint({required this.pos, required this.t, this.depthFt});
  final LatLng pos;
  final DateTime t;
  final double? depthFt;
}

/// 0 ft = red → 10 ft+ = green, linear — ported from the web app's
/// depthToColor (src/marineMap.svelte:2696).
Color depthToColor(double depthFt, {double opacity = 1.0}) {
  final t = (depthFt / 10).clamp(0.0, 1.0);
  return Color.fromRGBO((255 * (1 - t)).round(), (255 * t).round(), 0, opacity);
}

/// Plain track colour when depth colouring is off (web: rgba(0,0,255,1)).
const Color trackColor = Color.fromRGBO(0, 0, 255, 1);

/// Rolling own-boat track: appends when the boat has moved at least
/// [minMoveMeters] (a moored boat doesn't accumulate thousands of identical
/// points), pruned oldest-first past [cap] (gap C5 — a track without pruning
/// isn't shippable). ~6 h of 1 Hz movement at the default cap.
class Track {
  Track({this.minMoveMeters = 3, this.cap = 20000});

  final double minMoveMeters;
  final int cap;
  final List<TrackPoint> points = [];
  static const Distance _distance = Distance();

  void record(LatLng pos, {double? depthFt, DateTime? at}) {
    if (points.isNotEmpty &&
        _distance.distance(points.last.pos, pos) < minMoveMeters) {
      return;
    }
    points.add(TrackPoint(pos: pos, t: at ?? DateTime.now(), depthFt: depthFt));
    if (points.length > cap) {
      points.removeRange(0, points.length - cap);
    }
  }

  void clear() => points.clear();

  /// Prepend cloud-recorded history (C3): only points strictly older than
  /// the first live point are taken, so the recorded tail meets the live
  /// head with no duplicates and no visible seam. Still bounded by [cap]
  /// (oldest dropped first, same as live pruning).
  void seed(List<TrackPoint> recorded) {
    if (recorded.isEmpty) return;
    final firstLive = points.isEmpty ? null : points.first.t;
    final older = firstLive == null
        ? recorded
        : [
            for (final p in recorded)
              if (p.t.isBefore(firstLive)) p
          ];
    if (older.isEmpty) return;
    points.insertAll(0, older);
    if (points.length > cap) {
      points.removeRange(0, points.length - cap);
    }
  }
}

/// Segment list for drawing: consecutive point pairs with the colour for the
/// chosen mode. flutter_map has no per-vertex gradients, so depth colouring
/// is one polyline per segment; adjacent same-colour segments are merged so
/// a long constant-depth (or non-coloured) run costs one polyline.
List<({List<LatLng> points, Color color})> trackSegments(
  List<TrackPoint> points, {
  required bool colorByDepth,
}) {
  if (points.length < 2) return const [];
  final out = <({List<LatLng> points, Color color})>[];
  Color colorAt(int i) {
    final d = points[i].depthFt;
    return (colorByDepth && d != null) ? depthToColor(d) : trackColor;
  }

  var run = <LatLng>[points[0].pos];
  var runColor = colorAt(0);
  for (var i = 1; i < points.length; i++) {
    final c = colorAt(i);
    if (c == runColor) {
      run.add(points[i].pos);
    } else {
      run.add(points[i].pos); // close the run at this vertex
      out.add((points: run, color: runColor));
      run = <LatLng>[points[i].pos];
      runColor = c;
    }
  }
  if (run.length >= 2) out.add((points: run, color: runColor));
  return out;
}
