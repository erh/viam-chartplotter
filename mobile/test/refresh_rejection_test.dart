import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/auth/viam_session.dart';

// The rule this guards: only a refusal by the identity provider may delete a
// stored session. Everything else — offline at launch, DNS not up, captive
// portal, a timeout — must keep the tokens, because clearing them forces a
// full browser sign-in that the user experiences as "it logged me out again".
void main() {
  group('refresh failure classification', () {
    test('OAuth refusals are recognised as rejections', () {
      for (final e in [
        Exception('invalid_grant: refresh token is invalid'),
        Exception('token request failed: INVALID_GRANT'),
        Exception('{"error":"invalid_token"}'),
        Exception('unauthorized_client'),
        Exception('invalid_client: unknown client'),
      ]) {
        expect(ViamSession.isRefreshRejectedForTesting(e), isTrue, reason: '$e');
      }
    });

    test('reachability failures are NOT rejections', () {
      for (final e in [
        TimeoutException('token endpoint timed out'),
        Exception('SocketException: Failed host lookup: auth.viam.com'),
        Exception('Connection closed before full header was received'),
        Exception('Network is unreachable'),
        Exception('HandshakeException: Connection terminated during handshake'),
        Exception('502 Bad Gateway'),
        Exception('Software caused connection abort'),
        StateError('token refresh failed: null'),
      ]) {
        expect(ViamSession.isRefreshRejectedForTesting(e), isFalse, reason: '$e');
      }
    });

    test('an unrecognised error defaults to transient, not rejection', () {
      // The safe direction: a wrong guess here costs a retry, whereas guessing
      // "rejected" costs the user a sign-in.
      expect(ViamSession.isRefreshRejectedForTesting(Exception('something odd')), isFalse);
    });
  });
}
