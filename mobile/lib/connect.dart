import 'dart:async';
import 'dart:convert';

import 'package:viam_sdk/viam_sdk.dart';

import 'auth/token_store.dart';

/// Storage key for the last-connected machine (`{robotId, orgId, name, fqdn}`),
/// written by the machine picker and read for launch auto-connect and for
/// re-dialing after the connection dies (e.g. phone lock). `fqdn` is the
/// resolved main-part address, cached so a re-dial can skip the app.viam.com
/// lookup; records written by older builds don't have it.
const kLastMachineKey = 'last_machine';

/// Dials a machine's main part via app.viam.com signaling with a per-machine
/// robot-owner API key — minted through the app API on first use, cached in
/// [TokenStore], re-minted once if the stored key stops working, one retry
/// on a dial timeout. Machine API keys are the credential signaling reliably
/// accepts: it hangs on our OAuth app's access token, and robot secrets are
/// deprecated (empty/disabled on newer machines).
///
/// No local/mDNS attempt: viam-server's `_rpc._tcp` advertisement is its
/// internal signaling listener bound to the MACHINE'S OWN loopback (a
/// 127.0.0.1 A-record and an ephemeral port), usable only by clients on the
/// boat itself — and the Dart SDK both dials it from off-box and mutates its
/// dial options on the mDNS attempt so its own fallback redials the same
/// dead endpoint. Cloud signaling is only the handshake — ICE still picks a
/// direct LAN path when both peers are on the boat network.
class MachineConnector {
  MachineConnector({required this.viam});

  final Viam viam;
  final TokenStore _storage = TokenStore();

  /// The main-part address this connector resolved (or was handed). Callers
  /// persist it alongside the machine so the next dial can skip the lookup.
  String? fqdn;

  /// Dial [robotId]. [cachedFqdn] skips the app.viam.com part lookup — worth
  /// doing on every re-dial, because two cloud round trips ahead of the
  /// handshake are the part most likely to fail on the marginal link that
  /// caused the reconnect in the first place.
  Future<RobotClient> connect(
    String robotId,
    String orgId, {
    String? cachedFqdn,
  }) async {
    var address = cachedFqdn ?? await _lookupFqdn(robotId);
    fqdn = address;
    final stored = await _loadStoredKey(robotId);
    if (stored != null) {
      try {
        return await _dialWithKey(address, stored);
      } catch (e) {
        if (!_isAuthFailure(e)) {
          // The credential is fine, the link is not — or a cached address has
          // gone stale. Re-resolve if we were dialing a cached address, then
          // retry once with the same key. Minting a replacement we don't need
          // costs a cloud round trip on every flaky-link retry and litters the
          // org with single-use keys.
          if (cachedFqdn != null) {
            address = await _lookupFqdn(robotId);
            fqdn = address;
          }
          return await _dialWithKey(address, stored);
        }
        // Key revoked/rotated server-side — discard and mint a fresh one.
        try {
          await _storage.delete(key: _storageKey(robotId));
        } catch (_) {}
      }
    }
    final minted = await _mintKey(robotId, orgId);
    try {
      return await _dialWithKey(address, minted);
    } on TimeoutException {
      // Signaling/answerer hiccups showed up as one-off 10 s dial timeouts
      // during testing while the identical dial succeeded moments later —
      // one retry before surfacing the error.
      return await _dialWithKey(address, minted);
    }
  }

  Future<String> _lookupFqdn(String robotId) async {
    final parts = await viam.appClient.listRobotParts(robotId);
    return parts.firstWhere((p) => p.mainPart).fqdn;
  }

  /// Whether a failed dial means the credential was rejected, as opposed to a
  /// network/timeout blip. Only the former should discard the cached key.
  static bool _isAuthFailure(Object e) {
    if (e is TimeoutException) return false;
    final s = e.toString().toLowerCase();
    return s.contains('unauthenticated') ||
        s.contains('permission denied') ||
        s.contains('permission_denied') ||
        s.contains('unauthorized');
  }

  static String _storageKey(String robotId) => 'machine_api_key_$robotId';

  Future<({String id, String key})?> _loadStoredKey(String robotId) async {
    try {
      final raw = await _storage.read(key: _storageKey(robotId));
      if (raw == null) return null;
      final m = jsonDecode(raw) as Map<String, dynamic>;
      final id = m['id'];
      final key = m['key'];
      if (id is String && key is String) return (id: id, key: key);
    } catch (_) {
      // Unreadable storage or corrupt entry — treat as no stored key.
    }
    return null;
  }

  Future<({String id, String key})> _mintKey(String robotId, String orgId) async {
    final resp = await viam.appClient.createKey(
      [
        ViamAuthorization(
          authorizationId: AuthorizationId.robotOwner,
          resourceType: ResourceType.robot,
          resourceId: robotId,
          organizationId: orgId,
          identityType: IdentityType.apiKey,
        ),
      ],
      'chartplotter-mobile',
    );
    try {
      await _storage.write(
        key: _storageKey(robotId),
        value: jsonEncode({'id': resp.id, 'key': resp.key}),
      );
    } catch (_) {
      // Storage unavailable — the key still works for this run.
    }
    return (id: resp.id, key: resp.key);
  }

  Future<RobotClient> _dialWithKey(String fqdn, ({String id, String key}) k) {
    final options = RobotClientOptions.withApiKey(k.id, k.key)
      ..dialOptions.attemptMdns = false;
    return RobotClient.atAddress(fqdn, options);
  }
}
