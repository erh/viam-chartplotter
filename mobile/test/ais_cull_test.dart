import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/ais.dart';
import 'package:viam_chartplotter_mobile/map/ais_markers.dart';

AisBoat boat(double lat, double lng, [String mmsi = 'x']) => AisBoat(
      mmsi: mmsi,
      name: 'b$mmsi',
      location: LatLng(lat, lng),
      sogKn: 5,
    );

void main() {
  final viewport = LatLngBounds(
    const LatLng(41.0, -72.5), // SW
    const LatLng(41.5, -72.0), // NE
  );

  test('targets outside the margin are culled, inside kept', () {
    final r = cullAisTargets(
      boats: [
        boat(41.2, -72.2, 'in'),
        boat(45.0, -60.0, 'far'),
        // Just outside the raw bounds but inside the 30% margin.
        boat(41.55, -72.2, 'margin'),
      ],
      bounds: viewport,
    );
    expect(r.shown.map((b) => b.mmsi), containsAll(['in', 'margin']));
    expect(r.shown.map((b) => b.mmsi), isNot(contains('far')));
    expect(r.culled, 1);
    expect(r.capped, 0);
  });

  test('over the cap, closest to the reference win and the drop is counted',
      () {
    const center = LatLng(41.25, -72.25);
    final boats = [
      for (var i = 0; i < 40; i++)
        boat(41.25 + i * 0.005, -72.25, 'd$i'), // increasing distance north
    ];
    final r = cullAisTargets(
        boats: boats, bounds: viewport, reference: center, cap: 10);
    expect(r.shown.length, 10);
    expect(r.capped, greaterThan(0));
    expect(r.shown.first.mmsi, 'd0'); // nearest kept
    expect(r.shown.map((b) => b.mmsi), isNot(contains('d39'))); // farthest cut
  });

  test('no bounds means no cull, cap still applies', () {
    final boats = [for (var i = 0; i < 5; i++) boat(10.0 + i, 10, '$i')];
    final r = cullAisTargets(boats: boats, cap: 3);
    expect(r.culled, 0);
    expect(r.shown.length, 3);
    expect(r.capped, 2);
  });

  test('panning away and back yields the same targets', () {
    final boats = [boat(41.2, -72.2, 'a'), boat(41.3, -72.3, 'b')];
    final away = LatLngBounds(const LatLng(30, -80), const LatLng(31, -79));
    final gone = cullAisTargets(boats: boats, bounds: away);
    expect(gone.shown, isEmpty);
    final back = cullAisTargets(boats: boats, bounds: viewport);
    expect(back.shown.map((b) => b.mmsi), containsAll(['a', 'b']));
  });
}
