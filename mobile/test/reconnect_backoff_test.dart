import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/viam_connection.dart';

void main() {
  group('reconnect backoff', () {
    test('first retry is quick so a blip recovers fast', () {
      // The first re-dial itself is immediate (no wait is consulted until an
      // attempt has failed); the wait after one failure stays short.
      expect(ViamConnection.reconnectBackoff(1), const Duration(seconds: 2));
      expect(ViamConnection.reconnectBackoff(2), const Duration(seconds: 4));
    });

    test('grows monotonically', () {
      var prev = Duration.zero;
      for (var n = 1; n <= 8; n++) {
        final d = ViamConnection.reconnectBackoff(n);
        expect(d >= prev, isTrue, reason: 'attempt $n went backwards');
        prev = d;
      }
    });

    test('saturates instead of growing without bound', () {
      expect(ViamConnection.reconnectBackoff(50),
          ViamConnection.reconnectBackoff(6));
      expect(ViamConnection.reconnectBackoff(50).inSeconds, lessThanOrEqualTo(30));
    });

    test('a zero/negative attempt count is clamped, not an index error', () {
      expect(ViamConnection.reconnectBackoff(0), const Duration(seconds: 2));
      expect(ViamConnection.reconnectBackoff(-3), const Duration(seconds: 2));
    });
  });
}
