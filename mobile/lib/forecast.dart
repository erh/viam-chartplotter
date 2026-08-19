import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;
import 'package:latlong2/latlong.dart';

/// Local weather forecast (G2), ported from the web app
/// (src/lib/WeatherOverlays.svelte refreshWeather): Open-Meteo called
/// directly — no API key, no Go-module proxy — with °F / knots / inches
/// requested on the wire so no unit conversion happens client-side.

/// One Open-Meteo snapshot for the drawer: current conditions plus the next
/// 4 h of rain and the sun times. [sunrise]/[sunset] follow the web rule:
/// today's pair, but once today's sunset has passed, tomorrow's — so the
/// panel stays useful all night (that is why the query asks forecast_days=2).
class LocalForecast {
  const LocalForecast({
    required this.tempF,
    required this.windKn,
    required this.windDirDeg,
    required this.rain4hIn,
    required this.sunrise,
    required this.sunset,
    required this.fetchedAt,
    required this.position,
  });

  final double? tempF;
  final double? windKn;
  final double? windDirDeg;

  /// Sum of the hourly precipitation values (forecast_hours=4), inches.
  final double rain4hIn;

  /// Local wall-clock times: Open-Meteo's timezone=auto returns ISO strings
  /// without a tz suffix already in the boat's local zone, and — like the
  /// web app — we parse them as local on purpose.
  final DateTime? sunrise;
  final DateTime? sunset;

  final DateTime fetchedAt;

  /// Where the fetch was made, so [shouldRefresh] can detect the boat
  /// sailing away from the forecast point.
  final LatLng position;

  /// Old enough that the UI must label it as stale rather than current.
  bool staleAt(DateTime now) =>
      now.difference(fetchedAt) > const Duration(minutes: 30);
}

double? _num(dynamic v) => v is num ? v.toDouble() : null;

/// Parse an Open-Meteo /v1/forecast body. Pure so tests can feed canned
/// JSON; [now] drives the sunset rule (defaults to the real clock) and
/// [position]/[fetchedAt] are stamped through from the caller.
LocalForecast parseForecast(
  String body, {
  DateTime? now,
  LatLng position = const LatLng(0, 0),
}) {
  final clock = now ?? DateTime.now();
  final decoded = jsonDecode(body);
  if (decoded is! Map) {
    throw const FormatException('forecast JSON is not an object');
  }

  final cur = decoded['current'];
  final tempF = cur is Map ? _num(cur['temperature_2m']) : null;
  final windKn = cur is Map ? _num(cur['wind_speed_10m']) : null;
  final windDirDeg = cur is Map ? _num(cur['wind_direction_10m']) : null;

  var rain = 0.0;
  final hourly = decoded['hourly'];
  if (hourly is Map && hourly['precipitation'] is List) {
    for (final v in hourly['precipitation'] as List) {
      if (v is num) rain += v.toDouble();
    }
  }

  // Web parity: [0] is today in the requested tz, but once today's sunset
  // has passed show tomorrow's pair instead.
  DateTime? sunAt(dynamic arr, int i) {
    if (arr is! List || i >= arr.length) return null;
    final s = arr[i];
    return s is String ? DateTime.tryParse(s) : null;
  }

  final daily = decoded['daily'];
  final sunriseArr = daily is Map ? daily['sunrise'] : null;
  final sunsetArr = daily is Map ? daily['sunset'] : null;
  var dayIdx = 0;
  final todaySunset = sunAt(sunsetArr, 0);
  if (todaySunset != null && todaySunset.isBefore(clock)) dayIdx = 1;

  return LocalForecast(
    tempF: tempF,
    windKn: windKn,
    windDirDeg: windDirDeg,
    rain4hIn: rain,
    sunrise: sunAt(sunriseArr, dayIdx),
    sunset: sunAt(sunsetArr, dayIdx),
    fetchedAt: clock,
    position: position,
  );
}

/// True when a fetch is due: never fetched, data older than [maxAge], or the
/// boat has moved more than [moveKm] from where the forecast was taken —
/// mirrors the web's "~6 nm or 30 min" refetch, tightened for the drawer.
bool shouldRefresh({
  LocalForecast? last,
  required LatLng pos,
  required DateTime now,
  Duration maxAge = const Duration(minutes: 15),
  double moveKm = 5,
}) {
  if (last == null) return true;
  if (now.difference(last.fetchedAt) > maxAge) return true;
  return const Distance().distance(last.position, pos) / 1000.0 > moveKm;
}

/// Fetches and caches the local forecast. On failure [last] survives so the
/// UI can show stale-but-labelled values instead of blanks; the error is
/// rethrown for the caller to decide what to surface.
class ForecastService {
  ForecastService({http.Client? client}) : _client = client;

  final http.Client? _client;

  LocalForecast? last;

  /// The cached forecast can no longer be presented as current.
  bool get stale {
    final l = last;
    return l == null || l.staleAt(DateTime.now());
  }

  Future<LocalForecast> fetch(double lat, double lng) async {
    // Same query the web app builds — the card's contract, verbatim.
    final uri = Uri.parse('https://api.open-meteo.com/v1/forecast'
        '?latitude=${lat.toStringAsFixed(4)}&longitude=${lng.toStringAsFixed(4)}'
        '&current=temperature_2m,wind_speed_10m,wind_direction_10m'
        '&hourly=temperature_2m,precipitation,wind_speed_10m'
        '&daily=sunrise,sunset'
        '&temperature_unit=fahrenheit&wind_speed_unit=kn&precipitation_unit=inch'
        '&timezone=auto&forecast_hours=4&forecast_days=2');
    // Without a timeout a dead cell link leaves the drawer spinning forever.
    final resp = await (_client ?? http.Client())
        .get(uri)
        .timeout(const Duration(seconds: 15));
    if (resp.statusCode != 200) {
      throw http.ClientException('forecast ${resp.statusCode}', uri);
    }
    return last = parseForecast(resp.body, position: LatLng(lat, lng));
  }
}
