import 'package:flutter/foundation.dart';
import 'package:flutter_appauth/flutter_appauth.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'oauth_config.dart';
import 'token_store.dart';

enum AuthStatus { unknown, signedOut, signingIn, signedIn, error }

/// Viam rejected the refresh token itself (revoked, expired, rotated out), as
/// opposed to us failing to reach Viam. Distinguishing the two is the whole
/// point: only a rejection may delete the stored session.
class _RefreshRejected implements Exception {
  _RefreshRejected(this.detail);
  final String detail;
  @override
  String toString() => 'refresh rejected: $detail';
}

/// Owns the user's app.viam.com session: the OAuth login, token persistence
/// and refresh, and the authenticated [Viam] handle the rest of the app uses
/// to list machines and open robot connections.
///
/// This is the piece the web app never needed — it ran embedded in
/// app.viam.com and inherited a `userToken` cookie. A standalone mobile app
/// owns the whole flow itself.
class ViamSession extends ChangeNotifier {
  static const _kAccess = 'viam_access_token';
  static const _kRefresh = 'viam_refresh_token';
  static const _kExpiry = 'viam_token_expiry';

  final FlutterAppAuth _appAuth = const FlutterAppAuth();
  final TokenStore _store = TokenStore();

  AuthStatus status = AuthStatus.unknown;
  String? error;
  Viam? viam;

  String? _accessToken;
  String? _refreshToken;
  DateTime? _expiry;

  bool get isSignedIn => status == AuthStatus.signedIn && viam != null;

  /// Called once on startup: restore a stored session (refreshing if the
  /// access token has expired) so the user doesn't re-auth every launch.
  Future<void> restore() async {
    if (!OAuthConfig.configured) {
      status = AuthStatus.signedOut;
      notifyListeners();
      return;
    }
    try {
      _accessToken = await _store.read(key: _kAccess);
      _refreshToken = await _store.read(key: _kRefresh);
      final exp = await _store.read(key: _kExpiry);
      _expiry = exp == null ? null : DateTime.tryParse(exp);
    } catch (_) {
      // Secure storage unavailable (e.g. macOS keychain entitlement missing on
      // a locally-signed build) — treat as no stored session rather than
      // crashing/hanging startup.
      _accessToken = _refreshToken = null;
      _expiry = null;
    }

    // Either token is enough to try with: a partially-written store (the
    // access token saved, the refresh token not, or vice versa) should still
    // get a chance rather than going straight to the browser.
    if (_accessToken == null && _refreshToken == null) {
      _set(AuthStatus.signedOut);
      return;
    }
    await _establish();
  }

  /// Bring the session up from whatever tokens we hold. Split out of [restore]
  /// so [retryRestore] can re-run it without a browser round trip.
  Future<void> _establish() async {
    try {
      if (_isExpired) {
        if (_refreshToken == null) {
          await _forgetSession('Session expired — please sign in again.');
          return;
        }
        await _refreshWithRetry();
      }
      if (_accessToken == null) {
        await _forgetSession('Session expired — please sign in again.');
        return;
      }
      await _buildViam();
      error = null;
      _set(AuthStatus.signedIn);
    } on _RefreshRejected catch (e) {
      // Viam actively rejected these credentials (revoked, expired refresh
      // token, rotated out). This is the ONLY case that justifies throwing the
      // stored session away.
      await _forgetSession(
          'Signed out by Viam — please sign in again. (${e.detail})');
    } catch (_) {
      // Transient: offline at launch, Wi-Fi still associating, captive portal,
      // DNS not up yet. Keep the tokens — clearing them here is what turned a
      // dropped packet at startup into a forced browser re-login every time.
      error = "Couldn't reach Viam to restore your session. "
          'Check your connection and try again.';
      _set(AuthStatus.signedOut);
    }
  }

  /// Retry a restore that failed for a reachability reason, reusing the stored
  /// tokens. Offered on the login screen so a bad moment at launch costs a tap
  /// rather than a full sign-in.
  Future<void> retryRestore() async {
    // Resume fires this too, so it can arrive while one is already running.
    if (!canRetryRestore || status == AuthStatus.signingIn) return;
    _set(AuthStatus.signingIn);
    await _establish();
  }

  /// True when a stored session is still on disk and only failed to come up
  /// because we couldn't reach Viam — i.e. [retryRestore] is worth offering.
  bool get canRetryRestore =>
      status != AuthStatus.signedIn && _refreshToken != null;

  bool _persistenceFailed = false;

  /// A signed-in session that nonetheless won't survive — surfaced in the app
  /// rather than left for the user to rediscover at the next launch, since
  /// both causes look identical from the outside ("it made me log in again")
  /// and neither is fixable without knowing which one it is.
  String? get warning {
    if (status != AuthStatus.signedIn) return null;
    if (_refreshToken == null) {
      return 'No refresh token is stored for this session, so it ends when '
          'the access token expires and you will have to sign in again. If '
          'this keeps happening, check that the OAuth app grants the '
          'offline_access scope.';
    }
    if (_persistenceFailed) {
      return 'This device would not store the session, so quitting the app '
          'means signing in again.';
    }
    return null;
  }

  /// Launch the system-browser OAuth flow (authorization code + PKCE).
  Future<void> signIn() async {
    if (!OAuthConfig.configured) {
      error = 'OAuth not configured (set VIAM_OAUTH_* dart-defines)';
      _set(AuthStatus.error);
      return;
    }
    _set(AuthStatus.signingIn);
    try {
      final result = await _appAuth.authorizeAndExchangeCode(
        AuthorizationTokenRequest(
          OAuthConfig.clientId,
          OAuthConfig.redirectUrl,
          issuer: OAuthConfig.issuer,
          scopes: OAuthConfig.scopes,
          // System browser + PKCE are flutter_appauth defaults; PKCE lets us
          // run as a public client with no secret.
          promptValues: const ['login'],
        ),
      );
      await _persist(
        access: result.accessToken,
        refresh: result.refreshToken,
        expiry: result.accessTokenExpirationDateTime,
      );
      await _buildViam();
      error = null;
      _set(AuthStatus.signedIn);
    } catch (e) {
      error = 'Sign-in failed: $e';
      _set(AuthStatus.error);
    }
  }

  Future<void> signOut() async {
    await _clear();
    viam = null;
    _set(AuthStatus.signedOut);
  }

  /// Hand back a valid access token, refreshing first if it's expired. The
  /// machine picker / connection code calls this before long-lived work.
  ///
  /// Throws on a transient refresh failure so the caller (the reconnect
  /// backoff) retries; a rejection by Viam signs out locally and returns null.
  Future<String?> validAccessToken() async {
    if (_isExpired && _refreshToken != null) {
      try {
        await _refresh();
        await _buildViam();
      } on _RefreshRejected catch (e) {
        await _forgetSession(
            'Signed out by Viam — please sign in again. (${e.detail})');
        return null;
      }
    }
    return _accessToken;
  }

  // ---- internals -----------------------------------------------------------

  bool get _isExpired =>
      _expiry == null ||
      DateTime.now().isAfter(_expiry!.subtract(const Duration(seconds: 60)));

  /// Launch routinely beats the network by a second or two (Wi-Fi still
  /// associating, cellular radio waking). One immediate attempt plus two short
  /// retries turns "had to sign in again" into a brief pause.
  static const _refreshRetries = <Duration>[
    Duration(seconds: 1),
    Duration(seconds: 3),
  ];

  Future<void> _refreshWithRetry() async {
    Object? last;
    for (var attempt = 0; attempt <= _refreshRetries.length; attempt++) {
      if (attempt > 0) await Future<void>.delayed(_refreshRetries[attempt - 1]);
      try {
        await _refresh();
        return;
      } on _RefreshRejected {
        rethrow; // a rejection won't become an acceptance on retry
      } catch (e) {
        last = e;
      }
    }
    throw StateError('token refresh failed: $last');
  }

  /// Coalesces concurrent refreshes. The reconnect loop calls
  /// [validAccessToken] on every re-dial, and with rotating refresh tokens two
  /// refreshes in flight at once would race — the loser's token is already
  /// invalidated, which would sign the user out mid-passage.
  Future<void>? _refreshInFlight;

  Future<void> _refresh() {
    final inFlight = _refreshInFlight;
    if (inFlight != null) return inFlight;
    final f = _doRefresh().whenComplete(() {
      _refreshInFlight = null;
    });
    _refreshInFlight = f;
    return f;
  }

  Future<void> _doRefresh() async {
    try {
      final result = await _appAuth.token(
        TokenRequest(
          OAuthConfig.clientId,
          OAuthConfig.redirectUrl,
          issuer: OAuthConfig.issuer,
          refreshToken: _refreshToken,
          scopes: OAuthConfig.scopes,
        ),
      );
      await _persist(
        access: result.accessToken,
        refresh: result.refreshToken ?? _refreshToken,
        expiry: result.accessTokenExpirationDateTime,
      );
    } catch (e) {
      if (_isRefreshRejected(e)) throw _RefreshRejected('$e');
      rethrow;
    }
    if (_accessToken == null) {
      throw StateError('token endpoint returned no access token');
    }
  }

  /// Whether Viam actively rejected the refresh token, as opposed to us never
  /// reaching it. Deliberately narrow: anything not matched here is treated as
  /// transient and the stored session is kept, because a wrong guess in that
  /// direction costs a retry while a wrong guess the other way costs a login.
  static bool _isRefreshRejected(Object e) {
    final s = e.toString().toLowerCase();
    return s.contains('invalid_grant') ||
        s.contains('invalid_token') ||
        s.contains('unauthorized_client') ||
        s.contains('invalid_client');
  }

  /// Test seam for [_isRefreshRejected]: this classification decides whether a
  /// failed refresh costs the user a retry or a full sign-in, so it is worth
  /// pinning down. Not for use outside tests.
  @visibleForTesting
  static bool isRefreshRejectedForTesting(Object e) => _isRefreshRejected(e);

  Future<void> _buildViam() async {
    final token = _accessToken;
    if (token == null) throw StateError('no access token');
    // Viam.withAccessToken attaches the token to every gRPC call's metadata.
    viam = Viam.withAccessToken(token);
  }

  /// Some token endpoints omit `expires_in`. Assume the OAuth-conventional
  /// hour (less a margin) rather than treating an unknown expiry as expired —
  /// doing the latter put a network round trip, and a chance to fail, in front
  /// of every single launch.
  static const _assumedTokenLifetime = Duration(minutes: 55);

  Future<void> _persist({
    String? access,
    String? refresh,
    DateTime? expiry,
  }) async {
    _accessToken = access;
    _refreshToken = refresh;
    _expiry = expiry ?? DateTime.now().add(_assumedTokenLifetime);
    // Persist best-effort: if storage is unavailable the in-memory session
    // still works for this run, it just won't survive a restart — which is
    // worth telling the user about instead of letting them rediscover it at
    // every launch.
    var stored = true;
    try {
      if (access != null) {
        stored &= await _store.write(key: _kAccess, value: access);
      }
      if (refresh != null) {
        stored &= await _store.write(key: _kRefresh, value: refresh);
      }
      stored &= await _store.write(
        key: _kExpiry,
        value: _expiry!.toIso8601String(),
      );
    } catch (_) {
      stored = false;
    }
    _persistenceFailed = !stored;
  }

  /// Drop the stored session and land on the login screen. Only for a real
  /// sign-out or a credential Viam has rejected — never for "we couldn't
  /// reach the network", which is what [_establish] keeps the tokens for.
  Future<void> _forgetSession(String message) async {
    await _clear();
    viam = null;
    error = message;
    _set(AuthStatus.signedOut);
  }

  Future<void> _clear() async {
    _accessToken = _refreshToken = null;
    _expiry = null;
    try {
      await _store.delete(key: _kAccess);
      await _store.delete(key: _kRefresh);
      await _store.delete(key: _kExpiry);
    } catch (_) {}
  }

  void _set(AuthStatus s) {
    status = s;
    notifyListeners();
  }
}
