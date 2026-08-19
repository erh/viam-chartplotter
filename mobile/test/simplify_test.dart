import 'dart:math' as math;

import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/simplify.dart';

// simplify.test.ts translated (E3).

// One degree of latitude ≈ this many meters for R = 6371008.8 (2πR/360).
const degLatM = 111194.93;

void main() {
  group('haversineMeters', () {
    test('is zero for identical points', () {
      expect(
          haversineMeters(const LatLng(40, -70), const LatLng(40, -70)), 0);
    });

    test('matches one degree of latitude', () {
      final d = haversineMeters(const LatLng(0, 0), const LatLng(1, 0));
      expect(d, closeTo(degLatM, 1));
    });

    test('shrinks a degree of longitude by cos(lat)', () {
      final atEquator =
          haversineMeters(const LatLng(0, 0), const LatLng(0, 1));
      final at60 = haversineMeters(const LatLng(60, 0), const LatLng(60, 1));
      // cos(60°) = 0.5, so the high-latitude degree is ~half as wide.
      expect(at60 / atEquator, closeTo(0.5, 0.01));
    });

    test('is symmetric', () {
      const a = LatLng(41.1, -71.5);
      const b = LatLng(41.4, -70.9);
      expect(haversineMeters(a, b), closeTo(haversineMeters(b, a), 1e-6));
    });
  });

  group('pathLengthMeters', () {
    test('is zero for fewer than two points', () {
      expect(pathLengthMeters(const []), 0);
      expect(pathLengthMeters(const [LatLng(1, 1)]), 0);
    });

    test('sums consecutive legs', () {
      const pts = [LatLng(0, 0), LatLng(1, 0), LatLng(2, 0)];
      expect(pathLengthMeters(pts), closeTo(2 * degLatM, 2));
    });
  });

  group('decimateByDistance', () {
    test('returns short inputs unchanged', () {
      const pts = [LatLng(0, 0), LatLng(1, 1)];
      expect(decimateByDistance(pts, 100), pts);
    });

    test('always keeps first and last', () {
      final pts = [
        for (var i = 0; i <= 100; i++) LatLng(40 + i * 0.00009, -70)
      ];
      final out = decimateByDistance(pts, 100);
      expect(out.first, pts.first);
      expect(out.last, pts.last);
    });

    test('spaces kept points by roughly the granularity', () {
      final pts = [
        for (var i = 0; i <= 100; i++) LatLng(40 + i * 0.00009, -70)
      ];
      // ~1000 m total / 100 m granularity → ~11 points (10 gaps + tail).
      expect(decimateByDistance(pts, 100).length, 11);
    });

    test('collapses sub-granularity jitter', () {
      final pts = [
        for (var i = 0; i < 100; i++)
          LatLng(40 + (i % 3) * 0.00001, -70 + (i % 2) * 0.00001),
        const LatLng(40.01, -70), // one far point
      ];
      // All jitter is within a few meters → first + far point only.
      expect(decimateByDistance(pts, 50).length, 2);
    });
  });

  group('douglasPeucker', () {
    test('collapses a straight line to its endpoints', () {
      final line = [
        for (var i = 0; i < 50; i++) LatLng(40 + i * 0.001, -70)
      ];
      expect(douglasPeucker(line, 5), hasLength(2));
    });

    test('preserves the corners of a square', () {
      const cornerLat = 0.009;
      final cornerLng = 0.009 / math.cos(40 * math.pi / 180);
      const a = LatLng(40, -70);
      final b = LatLng(40, -70 + cornerLng);
      final c = LatLng(40 + cornerLat, -70 + cornerLng);
      const d = LatLng(40 + cornerLat, -70);
      final sq = <LatLng>[];
      void edge(LatLng p, LatLng q) {
        for (var i = 0; i < 20; i++) {
          sq.add(LatLng(
            p.latitude + (q.latitude - p.latitude) * i / 20,
            p.longitude + (q.longitude - p.longitude) * i / 20,
          ));
        }
      }

      edge(a, b);
      edge(b, c);
      edge(c, d);
      edge(d, a);
      sq.add(a);
      // 4 corners + the closing return to A.
      expect(douglasPeucker(sq, 10), hasLength(5));
    });

    test('keeps more points as tolerance tightens', () {
      // A bump in the middle of an otherwise straight line.
      const pts = [
        LatLng(40, -70),
        LatLng(40.001, -69.999),
        LatLng(40.002, -70),
      ];
      expect(douglasPeucker(pts, 1000).length, 2); // within tolerance → drop
      expect(douglasPeucker(pts, 10).length, 3); // exceeds tolerance → keep
    });
  });

  group('simplifyTrack', () {
    test('drops invalid coordinates', () {
      final out = simplifyTrack(
        [
          const LatLng(double.nan, 0),
          // latlong2 refuses lng 200 at construction, so the out-of-range
          // case from the web test is expressed as an invalid latitude.
          const LatLng(double.nan, 100),
          const LatLng(40, -70),
          const LatLng(40.001, -70),
        ],
        const SimplifyOptions(granularityMeters: 1),
      );
      expect(out.inputCount, 2);
      expect(out.waypoints, hasLength(2));
      expect(out.waypoints.first, const LatLng(40, -70));
    });

    test('returns two-or-fewer point inputs untouched', () {
      final out = simplifyTrack(
          const [LatLng(1, 1)], const SimplifyOptions(granularityMeters: 100));
      expect(out.waypoints, hasLength(1));
      expect(out.capped, isFalse);
    });

    test('reduces a dense straight track to its endpoints', () {
      final pts = [
        for (var i = 0; i < 200; i++) LatLng(40 + i * 0.0001, -70)
      ];
      final out =
          simplifyTrack(pts, const SimplifyOptions(granularityMeters: 50));
      expect(out.waypoints, hasLength(2));
      expect(out.capped, isFalse);
    });

    test('caps output at maxPoints and flags it', () {
      // A high-amplitude zigzag DP can't reduce below its corner count, so
      // the maxPoints escalation must kick in.
      final pts = [
        for (var i = 0; i < 100; i++)
          LatLng(40 + i * 0.001, -70 + (i % 2) * 0.0005)
      ];
      final out = simplifyTrack(
          pts, const SimplifyOptions(granularityMeters: 1, maxPoints: 10));
      expect(out.capped, isTrue);
      expect(out.waypoints.length, lessThanOrEqualTo(10));
      expect(out.waypoints.length, greaterThanOrEqualTo(2));
    });

    test('does not cap when the route already fits', () {
      final pts = [
        for (var i = 0; i < 50; i++) LatLng(40 + i * 0.001, -70)
      ];
      final out = simplifyTrack(pts,
          const SimplifyOptions(granularityMeters: 10, maxPoints: 500));
      expect(out.capped, isFalse);
    });
  });
}
