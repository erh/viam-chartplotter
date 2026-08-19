import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;

import 'config.dart';
import 'settings.dart';

/// Runtime server configuration (A6) — the mobile equivalent of the web
/// app's `/app-config` fetch (src/marineMap.svelte:3500-3524, served by
/// module.go): `{"tileServerBaseURL": "https://…", "chartOnly": false}`.
///
/// `TILE_BASE` used to be compile-time only, so one build could only ever
/// talk to one tile server. Now the `--dart-define` (or hosted default) is
/// just the *probe* host: when its /app-config names a different
/// tileServerBaseURL, tile traffic moves there with no rebuild. The resolved
/// base is cached in Settings so the next launch starts on it immediately
/// instead of re-probing, and [tileBase] is a ValueNotifier so tile layers
/// rebuild live when it changes.
class AppConfig {
  /// Current tile/weather server base URL, no trailing slash.
  static final ValueNotifier<String> tileBase =
      ValueNotifier(Settings.instance.tileBaseOverride ?? Config.tileBase);

  /// Server is a chart-only kiosk: show the chart, poll no boat.
  static bool chartOnly = false;

  /// Probe `<default base>/app-config`. Unreachable or malformed → silent
  /// fallback to the current value, matching the web app.
  static Future<void> load() async {
    try {
      final r = await http
          .get(Uri.parse('${Config.tileBase}/app-config'))
          .timeout(const Duration(seconds: 5));
      if (r.statusCode != 200) return;
      final j = jsonDecode(r.body);
      if (j is! Map) return;
      chartOnly = j['chartOnly'] == true;
      final base = j['tileServerBaseURL'];
      if (base is String && base.isNotEmpty) {
        final trimmed =
            base.endsWith('/') ? base.substring(0, base.length - 1) : base;
        Settings.instance.tileBaseOverride = trimmed;
        if (tileBase.value != trimmed) tileBase.value = trimmed;
      }
    } catch (_) {
      // Offline / no module server — the default base keeps working.
    }
  }
}
