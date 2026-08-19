import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/chart/bbox_source.dart';

LatLngBounds bounds(double s, double w, double n, double e) =>
    LatLngBounds(LatLng(s, w), LatLng(n, e));

void main() {
  test('extentCovered: inside one loaded extent, not a union of two', () {
    final loaded = [(-73.0, 41.0, -72.0, 42.0)];
    expect(extentCovered(loaded, (-72.8, 41.2, -72.2, 41.8)), isTrue);
    expect(extentCovered(loaded, (-73.5, 41.2, -72.2, 41.8)), isFalse);
    // Two adjacent extents that only jointly cover the ask → not covered
    // (deliberate: union math is where the bugs live).
    final two = [(-73.0, 41.0, -72.5, 42.0), (-72.5, 41.0, -72.0, 42.0)];
    expect(extentCovered(two, (-72.9, 41.2, -72.1, 41.8)), isFalse);
  });

  test('panning back over loaded water issues no new request', () async {
    var calls = 0;
    final src = BboxFeatureSource<int>(
      fetch: (w, s, e, n) async {
        calls++;
        return [1, 2, 3];
      },
      debounce: Duration.zero,
    );
    final b = bounds(41.0, -72.5, 41.5, -72.0);
    src.viewportChanged(b);
    await Future<void>.delayed(const Duration(milliseconds: 20));
    expect(calls, 1);
    // A slightly-shifted view inside the padded extent: no refetch.
    src.viewportChanged(bounds(41.02, -72.48, 41.52, -72.02));
    await Future<void>.delayed(const Duration(milliseconds: 20));
    expect(calls, 1);
    // A genuinely new area fetches.
    src.viewportChanged(bounds(45.0, -60.0, 45.5, -59.5));
    await Future<void>.delayed(const Duration(milliseconds: 20));
    expect(calls, 2);
    expect(src.features.length, 6);
  });

  test('crossing the cap evicts and repopulates the current viewport',
      () async {
    var calls = 0;
    final src = BboxFeatureSource<int>(
      fetch: (w, s, e, n) async {
        calls++;
        return List.generate(60, (i) => i);
      },
      cap: 100,
      debounce: Duration.zero,
    );
    src.viewportChanged(bounds(41.0, -72.5, 41.5, -72.0));
    await Future<void>.delayed(const Duration(milliseconds: 20));
    expect(src.features.length, 60);
    src.viewportChanged(bounds(45.0, -60.0, 45.5, -59.5));
    await Future<void>.delayed(const Duration(milliseconds: 20));
    // 120 > cap → evicted, repopulated with just the current viewport.
    expect(src.features.length, 60);
    expect(calls, 3); // two loads + the repopulate
  });

  test('rapid pans issue a bounded number of requests', () async {
    var calls = 0;
    final src = BboxFeatureSource<int>(
      fetch: (w, s, e, n) async {
        calls++;
        await Future<void>.delayed(const Duration(milliseconds: 10));
        return const [];
      },
      debounce: Duration.zero,
    );
    // 20 distinct viewports as fast as the debounce allows.
    for (var i = 0; i < 20; i++) {
      src.viewportChanged(bounds(10.0 + i, 10.0, 10.5 + i, 10.5));
      await Future<void>.delayed(const Duration(milliseconds: 1));
    }
    await Future<void>.delayed(const Duration(milliseconds: 100));
    // In-flight coalescing: far fewer requests than viewport changes.
    expect(calls, lessThan(10));
  });
}
