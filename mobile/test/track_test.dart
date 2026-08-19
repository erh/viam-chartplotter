import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/track.dart';

void main() {
  test('depthToColor: 0 ft red, 10+ ft green, 5 ft in between', () {
    expect(depthToColor(0), const Color.fromRGBO(255, 0, 0, 1));
    expect(depthToColor(10), const Color.fromRGBO(0, 255, 0, 1));
    expect(depthToColor(50), const Color.fromRGBO(0, 255, 0, 1));
    final mid = depthToColor(5);
    expect((mid.r * 255).round(), 128);
    expect((mid.g * 255).round(), 128);
  });

  test('minimum-move filter: a moored boat does not accumulate points', () {
    final t = Track(minMoveMeters: 3);
    const dock = LatLng(41.3, -72.0);
    for (var i = 0; i < 1000; i++) {
      // GPS jitter well under 3 m.
      t.record(LatLng(dock.latitude + i % 2 * 1e-6, dock.longitude));
    }
    expect(t.points.length, 1);
  });

  test('movement accumulates and pruning caps memory oldest-first', () {
    final t = Track(minMoveMeters: 1, cap: 100);
    for (var i = 0; i < 250; i++) {
      t.record(LatLng(41.0 + i * 0.001, -72.0));
    }
    expect(t.points.length, 100);
    // Oldest pruned: the first surviving point is from late in the run.
    expect(t.points.first.pos.latitude, greaterThan(41.1));
  });

  test('segments merge same-colour runs and split on colour change', () {
    final base = DateTime(2026);
    final pts = [
      TrackPoint(pos: const LatLng(41.0, -72.0), t: base, depthFt: 20),
      TrackPoint(pos: const LatLng(41.001, -72.0), t: base, depthFt: 20),
      TrackPoint(pos: const LatLng(41.002, -72.0), t: base, depthFt: 20),
      TrackPoint(pos: const LatLng(41.003, -72.0), t: base, depthFt: 2),
      TrackPoint(pos: const LatLng(41.004, -72.0), t: base, depthFt: 2),
    ];
    final colored = trackSegments(pts, colorByDepth: true);
    expect(colored.length, 2);
    expect(colored.first.points.length, 4); // deep run + closing vertex
    expect(colored.last.color, depthToColor(2)); // shoal = red-ish

    // Depth mode off → one plain-colour polyline for everything.
    final plain = trackSegments(pts, colorByDepth: false);
    expect(plain.length, 1);
    expect(plain.single.color, trackColor);
    expect(plain.single.points.length, 5);
  });

  test('fewer than two points draws nothing', () {
    expect(trackSegments(const [], colorByDepth: true), isEmpty);
  });

  group('seed (C3 — recorded history prepends the live track)', () {
    TrackPoint tp(double lat, DateTime t) =>
        TrackPoint(pos: LatLng(lat, -72.0), t: t);
    final t0 = DateTime(2026, 8, 18, 6);

    test('recorded points land before live ones, no seam duplicates', () {
      final track = Track(minMoveMeters: 0);
      // Live points from "app launch" at 12:00.
      final launch = DateTime(2026, 8, 18, 12);
      track.record(const LatLng(41.5, -72.0), at: launch);
      track.record(const LatLng(41.6, -72.0),
          at: launch.add(const Duration(minutes: 1)));
      // Recorded history spans 06:00 → 12:30 — the overlap past launch
      // must be dropped, not interleaved.
      track.seed([
        tp(41.0, t0),
        tp(41.1, t0.add(const Duration(hours: 1))),
        tp(41.9, launch.add(const Duration(minutes: 30))), // overlaps live
      ]);
      expect([for (final p in track.points) p.pos.latitude],
          [41.0, 41.1, 41.5, 41.6]);
      // In chronological order end to end — that's the "no visible seam".
      final times = [for (final p in track.points) p.t];
      expect(times, List.of(times)..sort());
    });

    test('seeding an empty track just installs the history', () {
      final track = Track();
      track.seed([tp(41.0, t0), tp(41.1, t0.add(const Duration(hours: 1)))]);
      expect(track.points, hasLength(2));
    });

    test('respects the cap, dropping oldest first', () {
      final track = Track(minMoveMeters: 0, cap: 3);
      track.record(const LatLng(41.5, -72.0), at: DateTime(2026, 8, 18, 12));
      track.seed([
        for (var i = 0; i < 5; i++)
          tp(41.0 + i * 0.01, t0.add(Duration(minutes: i)))
      ]);
      expect(track.points, hasLength(3));
      // The newest recorded points + the live point survive.
      expect(track.points.last.pos.latitude, 41.5);
      expect(track.points.first.pos.latitude, closeTo(41.03, 1e-9));
    });
  });
}
