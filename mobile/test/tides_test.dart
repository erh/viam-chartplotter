import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/tides.dart';

TideSeriesPoint sp(int minutes, double v) =>
    TideSeriesPoint(DateTime(2026, 8, 19, 12).add(Duration(minutes: minutes)), v);

void main() {
  test('nearestTideStation: haversine picks the closer station', () {
    const stations = [
      TideStation(id: '8461490', name: 'New London', lat: 41.371, lng: -72.096),
      TideStation(id: '8510560', name: 'Montauk', lat: 41.048, lng: -71.960),
    ];
    final near = nearestTideStation(stations, 41.35, -72.09)!;
    expect(near.station.id, '8461490');
    expect(near.distNm, lessThan(2));
    expect(nearestTideStation(const [], 41, -72), isNull);
  });

  test('interpCurrent: linear between hourly points, clamped at ends', () {
    final series = [sp(0, 2.0), sp(60, 4.0)];
    expect(interpCurrent(series, DateTime(2026, 8, 19, 12, 30)), closeTo(3.0, 1e-9));
    expect(interpCurrent(series, DateTime(2026, 8, 19, 11)), 2.0);
    expect(interpCurrent(series, DateTime(2026, 8, 19, 14)), 4.0);
    expect(interpCurrent(const [], DateTime.now()), isNull);
  });

  test('clipSeries interpolates the window edges', () {
    final series = [sp(0, 0.0), sp(60, 6.0), sp(120, 0.0)];
    final t0 = DateTime(2026, 8, 19, 12, 30).millisecondsSinceEpoch;
    final t1 = DateTime(2026, 8, 19, 13, 30).millisecondsSinceEpoch;
    final clipped = clipSeries(series, t0, t1);
    expect(clipped.first.v, closeTo(3.0, 1e-9)); // interpolated edge
    expect(clipped.last.v, closeTo(3.0, 1e-9));
    expect(clipped.map((p) => p.v), contains(6.0)); // the interior peak
  });

  test('synthSeriesFromHilo: half-cosine hits both peaks', () {
    final high = TidePoint(t: DateTime(2026, 8, 19, 12), v: 6.0, type: 'H');
    final low = TidePoint(t: DateTime(2026, 8, 19, 18, 15), v: 0.5, type: 'L');
    final synth = synthSeriesFromHilo(
      [high, low],
      high.t.millisecondsSinceEpoch,
      low.t.millisecondsSinceEpoch,
    );
    expect(synth.first.v, closeTo(6.0, 1e-9));
    expect(synth.last.v, closeTo(0.5, 0.05)); // 15-min sampling lands near
    // Midpoint of a half-cosine is the mean of the peaks.
    final mid = synth[synth.length ~/ 2];
    expect(mid.v, closeTo(3.25, 0.35));
    expect(synthSeriesFromHilo([high], 0, 1), isEmpty);
  });

  test('fmtNoaaDate: yyyyMMdd', () {
    expect(fmtNoaaDate(DateTime(2026, 8, 9)), '20260809');
  });
}
