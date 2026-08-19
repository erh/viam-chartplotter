import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/routes/route_stats.dart';
import 'package:viam_chartplotter_mobile/routes/route_store.dart';

// E5 — full route stats, a port of the web's routeStats derivation.

NavWaypoint wp(String id, double lat, double lng) =>
    NavWaypoint(id: id, pos: LatLng(lat, lng));

void main() {
  const boat = LatLng(41.0, -71.0);
  final route = [
    wp('a', 41.1, -71.0),
    wp('b', 41.2, -71.0),
    wp('c', 41.3, -71.0),
  ];
  const d = Distance();

  test('Final distance equals boat→first plus the sum of remaining legs', () {
    final s = computeRouteStats(boat, route, 6.0)!;
    final expectedMeters = d.distance(boat, route[0].pos) +
        d.distance(route[0].pos, route[1].pos) +
        d.distance(route[1].pos, route[2].pos);
    expect(s.finalNm, closeTo(expectedMeters / 1852, 0.01));
    expect(s.nextNm, closeTo(d.distance(boat, route[0].pos) / 1852, 0.01));
    expect(s.waypointCount, 3);
    // ~0.1° legs due north: bearing to the next waypoint is ~0.
    expect(s.nextBearingDeg, closeTo(0, 1));
  });

  test('ETAs are consistent with SOG', () {
    final s = computeRouteStats(boat, route, 6.0)!;
    expect(s.nextMinutes, closeTo(s.nextNm / 6.0 * 60, 0.1));
    expect(s.finalMinutes, closeTo(s.finalNm / 6.0 * 60, 0.1));
  });

  test('both ETAs blank out (null, not zero) when stationary', () {
    for (final spd in [null, 0.0, 0.05]) {
      final s = computeRouteStats(boat, route, spd)!;
      expect(s.nextMinutes, isNull);
      expect(s.finalMinutes, isNull);
      expect(formatDurationMin(s.finalMinutes), '—');
      expect(formatEta(s.finalMinutes), '—');
    }
  });

  test('null without a boat fix, an empty route, or a null-island fix', () {
    expect(computeRouteStats(null, route, 5), isNull);
    expect(computeRouteStats(boat, const [], 5), isNull);
    expect(computeRouteStats(const LatLng(0, 0), route, 5), isNull);
  });

  group('formatDurationMin', () {
    test('minutes below an hour, h/m above', () {
      expect(formatDurationMin(45.4), '45 min');
      expect(formatDurationMin(125), '2h 5m');
      expect(formatDurationMin(120), '2h');
    });
  });

  group('formatEta', () {
    test('wall-clock arrival from a fixed now', () {
      final now = DateTime(2026, 8, 18, 13, 0);
      expect(formatEta(65, now: now), '14:05');
    });
  });
}
