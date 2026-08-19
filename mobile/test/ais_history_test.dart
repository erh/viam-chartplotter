import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/ais.dart';

void main() {
  test('parses samples and skips invalid Locations (web parity)', () {
    final pts = aisHistoryPoints([
      {'Location': [41.0, -72.0], 'Created': '2026-08-19T12:00:00Z'},
      {'Location': [41.1, -72.1], 'Timestamp': 'Tue, 19 Aug 2026'},
      {'Location': [41.2]}, // too short
      {'Location': 'nope'},
      null,
      {'Location': [double.nan, -72.0]}, // non-finite
      {'NoLocation': true},
    ]);
    expect(pts.length, 2);
    expect(pts.first.latitude, 41.0);
    expect(pts.last.longitude, -72.1);
  });

  test('non-list input yields nothing', () {
    expect(aisHistoryPoints(null), isEmpty);
    expect(aisHistoryPoints({'total': 3}), isEmpty);
  });
}
