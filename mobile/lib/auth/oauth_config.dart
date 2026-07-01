/// OAuth 2.0 configuration for "Sign in with Viam".
///
/// Viam authenticates users through its FusionAuth tenant; a standalone mobile
/// app must be registered as an OAuth client there (to get a `clientId` and to
/// whitelist the `redirectUrl` scheme). Those values are environment-specific
/// and not secrets, so they come in via --dart-define rather than being baked
/// in. Supply them like:
///
///   --dart-define=VIAM_OAUTH_ISSUER=https://auth.viam.com \
///   --dart-define=VIAM_OAUTH_CLIENT_ID=<client-id> \
///   --dart-define=VIAM_OAUTH_REDIRECT=com.viam.chartplotter:/oauthredirect
///
/// When `issuer` + `clientId` are absent the app skips login and falls back to
/// the API-key spike path (see Config), so it still runs without OAuth setup.
class OAuthConfig {
  /// OIDC issuer; the SDK discovers the authorize/token endpoints from
  /// `<issuer>/.well-known/openid-configuration`.
  static const String issuer = String.fromEnvironment('VIAM_OAUTH_ISSUER');

  /// Public client id of the OAuth app registered with Viam.
  static const String clientId = String.fromEnvironment('VIAM_OAUTH_CLIENT_ID');

  /// Custom-scheme redirect; must match the app registration AND the native
  /// platform config (Android manifest placeholder / iOS Info.plist URL type).
  static const String redirectUrl = String.fromEnvironment(
    'VIAM_OAUTH_REDIRECT',
    defaultValue: 'com.viam.chartplotter:/oauthredirect',
  );

  /// offline_access yields a refresh token so the session survives token
  /// expiry without forcing the user back through the browser.
  static const List<String> scopes = [
    'openid',
    'profile',
    'email',
    'offline_access',
  ];

  static bool get configured => issuer.isNotEmpty && clientId.isNotEmpty;
}
