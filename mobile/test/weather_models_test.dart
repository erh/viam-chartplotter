import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:viam_chartplotter_mobile/weather.dart';

// F2 — weather model catalogue: parsing, fh clamping, and the GFS fallback
// when /noaa-weather/models is unreachable.

void main() {
  group('parseWeatherModels', () {
    test('parses the server shape, dropping garbage entries', () {
      final models = parseWeatherModels('''
        [
          {"name":"gfs","displayName":"GFS","kind":"wind","minFh":0,"maxFh":240,"stepFh":3},
          {"name":"hrrr","displayName":"HRRR","kind":"wind","minFh":0,"maxFh":48,"stepFh":1,
           "disabled":true,"reason":"no recent run"},
          {"name":"gfswave","displayName":"GFS Wave","kind":"wave","minFh":0,"maxFh":120,"stepFh":3},
          {"noName":true},
          42
        ]''');
      expect(models, hasLength(3));
      expect(models[0].name, 'gfs');
      expect(models[1].disabled, isTrue);
      expect(models[1].reason, 'no recent run');
      expect(models[2].kind, 'wave');
    });

    test('throws on a non-array body', () {
      expect(() => parseWeatherModels('{"oops":1}'), throwsFormatException);
    });
  });

  group('clampFh', () {
    const hrrr = WeatherModel(
        name: 'hrrr',
        displayName: 'HRRR',
        kind: 'wind',
        minFh: 0,
        maxFh: 48,
        stepFh: 1);
    test('reshapes an out-of-range hour into the model range', () {
      expect(hrrr.clampFh(240), 48); // GFS hour → short-range max
      expect(hrrr.clampFh(-3), 0);
    });
    test('snaps to the model step', () {
      const gfs = WeatherModel.gfsFallback; // step 3
      expect(gfs.clampFh(7), 6);
      expect(gfs.clampFh(8), 9);
      expect(gfs.clampFh(300), 240);
    });
  });

  group('fetchWeatherModels', () {
    test('unreachable endpoint falls back to GFS defaults', () async {
      final client = MockClient((_) async => throw Exception('down'));
      final models =
          await fetchWeatherModels('http://x', client: client);
      expect(models, hasLength(1));
      expect(models.single.name, 'gfs');
      expect(models.single.maxFh, 240);
      expect(models.single.stepFh, 3);
    });

    test('HTTP error also falls back', () async {
      final client = MockClient((_) async => http.Response('nope', 500));
      final models = await fetchWeatherModels('http://x', client: client);
      expect(models.single.name, 'gfs');
    });

    test('good catalogue comes through', () async {
      final client = MockClient((_) async => http.Response(
          '[{"name":"hrrr","displayName":"HRRR","kind":"wind",'
          '"minFh":0,"maxFh":48,"stepFh":1}]',
          200));
      final models = await fetchWeatherModels('http://x', client: client);
      expect(models.single.name, 'hrrr');
      expect(models.single.maxFh, 48);
    });
  });
}
