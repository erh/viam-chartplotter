import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/forecast.dart';

/// A realistic Open-Meteo body for the exact query the service issues
/// (°F / kn / inch, forecast_hours=4, forecast_days=2, timezone=auto).
const payload = '''
{
  "latitude": 41.3,
  "longitude": -72.1,
  "timezone": "America/New_York",
  "current_units": {"temperature_2m": "°F", "wind_speed_10m": "kn"},
  "current": {
    "time": "2026-08-19T14:00",
    "temperature_2m": 78.3,
    "wind_speed_10m": 11.2,
    "wind_direction_10m": 225
  },
  "hourly": {
    "time": ["2026-08-19T14:00", "2026-08-19T15:00",
             "2026-08-19T16:00", "2026-08-19T17:00"],
    "temperature_2m": [78.3, 79.1, 79.8, 78.9],
    "precipitation": [0.0, 0.05, 0.1, 0.0],
    "wind_speed_10m": [11.2, 12.0, 12.5, 11.8]
  },
  "daily": {
    "time": ["2026-08-19", "2026-08-20"],
    "sunrise": ["2026-08-19T06:01", "2026-08-20T06:02"],
    "sunset": ["2026-08-19T19:45", "2026-08-20T19:43"]
  }
}
''';

void main() {
  final afternoon = DateTime(2026, 8, 19, 14); // before today's sunset
  final night = DateTime(2026, 8, 19, 20, 30); // after today's sunset

  test('parseForecast: current values and 4 h rain sum', () {
    final f = parseForecast(payload, now: afternoon);
    expect(f.tempF, 78.3);
    expect(f.windKn, 11.2);
    expect(f.windDirDeg, 225);
    expect(f.rain4hIn, closeTo(0.15, 1e-9));
    expect(f.fetchedAt, afternoon);
  });

  test('parseForecast: before sunset the sun row shows today', () {
    final f = parseForecast(payload, now: afternoon);
    expect(f.sunrise, DateTime(2026, 8, 19, 6, 1));
    expect(f.sunset, DateTime(2026, 8, 19, 19, 45));
  });

  test('parseForecast: after sunset the sun row rolls to tomorrow', () {
    final f = parseForecast(payload, now: night);
    expect(f.sunrise, DateTime(2026, 8, 20, 6, 2));
    expect(f.sunset, DateTime(2026, 8, 20, 19, 43));
  });

  test('parseForecast: missing sections give nulls, not a crash', () {
    final f = parseForecast('{"daily": {}}', now: afternoon);
    expect(f.tempF, isNull);
    expect(f.windKn, isNull);
    expect(f.windDirDeg, isNull);
    expect(f.rain4hIn, 0.0);
    expect(f.sunrise, isNull);
    expect(f.sunset, isNull);
  });

  group('shouldRefresh', () {
    const home = LatLng(41.3, -72.1);
    final f = parseForecast(payload, now: afternoon, position: home);

    test('fresh and nearby: no refetch', () {
      expect(
        shouldRefresh(
            last: f,
            pos: const LatLng(41.31, -72.11),
            now: afternoon.add(const Duration(minutes: 5))),
        isFalse,
      );
    });

    test('never fetched: refetch', () {
      expect(shouldRefresh(last: null, pos: home, now: afternoon), isTrue);
    });

    test('older than maxAge: refetch', () {
      expect(
        shouldRefresh(
            last: f, pos: home, now: afternoon.add(const Duration(minutes: 16))),
        isTrue,
      );
    });

    test('boat moved beyond moveKm: refetch', () {
      // ~0.1° of latitude is ~11 km, past the 5 km default.
      expect(
        shouldRefresh(
            last: f,
            pos: const LatLng(41.4, -72.1),
            now: afternoon.add(const Duration(minutes: 1))),
        isTrue,
      );
    });
  });

  test('staleAt: labelled stale past 30 minutes', () {
    final f = parseForecast(payload, now: afternoon);
    expect(f.staleAt(afternoon.add(const Duration(minutes: 29))), isFalse);
    expect(f.staleAt(afternoon.add(const Duration(minutes: 31))), isTrue);
  });

  test('fetch: success caches, later failure keeps the cache', () async {
    var fail = false;
    final client = MockClient((req) async {
      expect(req.url.host, 'api.open-meteo.com');
      expect(req.url.queryParameters['forecast_days'], '2');
      if (fail) throw http.ClientException('offline', req.url);
      return http.Response(payload, 200);
    });
    final svc = ForecastService(client: client);

    final f = await svc.fetch(41.3, -72.1);
    expect(svc.last, same(f));
    expect(f.position, const LatLng(41.3, -72.1));

    fail = true;
    await expectLater(svc.fetch(41.3, -72.1), throwsA(isA<Exception>()));
    expect(svc.last, same(f)); // dropout shows stale data, not blanks
  });

  test('fetch: non-200 throws and keeps the cache', () async {
    var status = 200;
    final client =
        MockClient((req) async => http.Response(payload, status));
    final svc = ForecastService(client: client);

    final f = await svc.fetch(41.3, -72.1);
    status = 502;
    await expectLater(
        svc.fetch(41.3, -72.1), throwsA(isA<http.ClientException>()));
    expect(svc.last, same(f));
  });
}
