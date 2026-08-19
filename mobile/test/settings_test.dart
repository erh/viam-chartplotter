import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:viam_chartplotter_mobile/settings.dart';

Future<Settings> settingsWith(Map<String, Object> values) async {
  SharedPreferences.setMockInitialValues(values);
  Settings.setForTesting(await SharedPreferences.getInstance());
  return Settings.instance;
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  test('round-trips the map view', () async {
    final s = await settingsWith({});
    expect(s.mapCenter, isNull);
    expect(s.mapZoom, isNull);
    s.mapCenter = const LatLng(41.3, -72.0);
    s.mapZoom = 11.5;
    expect(s.mapCenter!.latitude, closeTo(41.3, 1e-9));
    expect(s.mapCenter!.longitude, closeTo(-72.0, 1e-9));
    expect(s.mapZoom, 11.5);
  });

  test('garbage stored values fall back to defaults, no crash', () async {
    final s = await settingsWith({
      'mapViewCenter': 'not json at all',
      'mapViewZoom': -3.0,
      'mapHeadsUp': 'banana',
      'safeDepthFt': 9999,
    });
    expect(s.mapCenter, isNull);
    expect(s.mapZoom, isNull);
    expect(s.courseUp, isFalse);
    expect(s.safeDepthFt, isNull);
  });

  test('out-of-range center is rejected', () async {
    final s = await settingsWith({'mapViewCenter': '[400, 95]'});
    expect(s.mapCenter, isNull);
  });

  test('center is stored web-style as [lon, lat]', () async {
    final s = await settingsWith({'mapViewCenter': '[-72.0, 41.3]'});
    expect(s.mapCenter!.latitude, closeTo(41.3, 1e-9));
    expect(s.mapCenter!.longitude, closeTo(-72.0, 1e-9));
  });

  test('safe depth validates and clears', () async {
    final s = await settingsWith({});
    s.safeDepthFt = 6;
    expect(s.safeDepthFt, 6);
    s.safeDepthFt = null;
    expect(s.safeDepthFt, isNull);
    s.safeDepthFt = -2;
    expect(s.safeDepthFt, isNull);
  });
}
