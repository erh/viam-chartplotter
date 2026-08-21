import 'dart:convert';
import 'dart:io';

import 'package:flutter_secure_storage/flutter_secure_storage.dart';

/// Key-value store for auth material (session tokens, cached machine API
/// keys, last-connected machine).
///
/// Prefers the platform keychain, but falls back to a plain JSON file under
/// the app container when the keychain is unreachable. That fallback exists
/// because macOS dev builds deliberately drop the keychain entitlement (it
/// forces signing with a development certificate — see
/// tool/macos-entitlements.sh), which made every launch forget the session
/// and re-prompt for login. The file lives inside the app's sandbox
/// container, so it's per-app readable like app documents — weaker than the
/// keychain, acceptable for a dev build; properly signed builds never hit
/// the fallback.
class TokenStore {
  static final TokenStore _instance = TokenStore._();
  factory TokenStore() => _instance;
  TokenStore._();

  // first_unlock (not the default whenUnlocked): the default makes keychain
  // reads fail whenever iOS considers the device locked, and a transient
  // failure at launch reads as "no stored session" — i.e. a spurious trip to
  // the login screen with perfectly good tokens on disk.
  final FlutterSecureStorage _secure = const FlutterSecureStorage(
    iOptions: IOSOptions(
        accessibility: KeychainAccessibility.first_unlock_this_device),
    mOptions: MacOsOptions(
        accessibility: KeychainAccessibility.first_unlock_this_device),
  );
  Map<String, String>? _fileCache;

  // Same named-parameter shape as FlutterSecureStorage, so this is a
  // drop-in replacement at call sites.
  Future<String?> read({required String key}) async {
    try {
      final v = await _secure.read(key: key);
      if (v != null) return v;
    } catch (_) {
      // Keychain unavailable — fall through to the file.
    }
    return (await _load())[key];
  }

  /// Returns true when the value reached durable storage. A false here is why
  /// a session silently fails to survive a restart, so callers that care (the
  /// auth session) surface it rather than letting the user rediscover it at
  /// every launch.
  Future<bool> write({required String key, required String? value}) async {
    if (value == null) {
      await delete(key: key);
      return true;
    }
    try {
      await _secure.write(key: key, value: value);
      return true;
    } catch (_) {
      // Keychain unavailable — fall through to the file.
    }
    final m = await _load();
    m[key] = value;
    return _save(m);
  }

  Future<void> delete({required String key}) async {
    try {
      await _secure.delete(key: key);
    } catch (_) {}
    final m = await _load();
    if (m.remove(key) != null) await _save(m);
  }

  File? get _file {
    final home = Platform.environment['HOME'];
    if (home == null || home.isEmpty) return null;
    return File('$home/Library/Application Support/viam-chartplotter/auth.json');
  }

  Future<Map<String, String>> _load() async {
    final cached = _fileCache;
    if (cached != null) return cached;
    var m = <String, String>{};
    try {
      final f = _file;
      if (f != null && f.existsSync()) {
        final decoded = jsonDecode(await f.readAsString());
        if (decoded is Map) {
          m = decoded.map((k, v) => MapEntry(k.toString(), v.toString()));
        }
      }
    } catch (_) {
      // Unreadable/corrupt file — start empty.
    }
    _fileCache = m;
    return m;
  }

  /// Returns true when the map actually made it to disk. The in-memory cache
  /// is updated either way so the value works for the rest of this run.
  Future<bool> _save(Map<String, String> m) async {
    _fileCache = m;
    try {
      final f = _file;
      if (f == null) return false;
      f.parent.createSync(recursive: true);
      await f.writeAsString(jsonEncode(m));
      return true;
    } catch (_) {
      // Best-effort: an unwritable disk just means no persistence this run.
      return false;
    }
  }
}
