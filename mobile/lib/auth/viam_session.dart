import 'package:flutter/foundation.dart';
import 'package:flutter_appauth/flutter_appauth.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:viam_sdk/viam_sdk.dart';

import 'oauth_config.dart';

enum AuthStatus { unknown, signedOut, signingIn, signedIn, error }

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
  final FlutterSecureStorage _store = const FlutterSecureStorage();

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

    if (_accessToken == null) {
      _set(AuthStatus.signedOut);
      return;
    }
    try {
      if (_isExpired && _refreshToken != null) {
        await _refresh();
      }
      await _buildViam();
      _set(AuthStatus.signedIn);
    } catch (e) {
      await _clear();
      error = 'Session restore failed: $e';
      _set(AuthStatus.signedOut);
    }
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
  Future<String?> validAccessToken() async {
    if (_isExpired && _refreshToken != null) {
      await _refresh();
      await _buildViam();
    }
    return _accessToken;
  }

  // ---- internals -----------------------------------------------------------

  bool get _isExpired =>
      _expiry == null ||
      DateTime.now().isAfter(_expiry!.subtract(const Duration(seconds: 60)));

  Future<void> _refresh() async {
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
  }

  Future<void> _buildViam() async {
    final token = _accessToken;
    if (token == null) throw StateError('no access token');
    // Viam.withAccessToken attaches the token to every gRPC call's metadata.
    viam = Viam.withAccessToken(token);
  }

  Future<void> _persist({
    String? access,
    String? refresh,
    DateTime? expiry,
  }) async {
    _accessToken = access;
    _refreshToken = refresh;
    _expiry = expiry;
    // Persist best-effort: if storage is unavailable the in-memory session
    // still works for this run, it just won't survive a restart.
    try {
      if (access != null) await _store.write(key: _kAccess, value: access);
      if (refresh != null) await _store.write(key: _kRefresh, value: refresh);
      if (expiry != null) {
        await _store.write(key: _kExpiry, value: expiry.toIso8601String());
      }
    } catch (_) {}
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
