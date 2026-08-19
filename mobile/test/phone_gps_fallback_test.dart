import 'package:flutter_test/flutter_test.dart';
import 'package:latlong2/latlong.dart';
import 'package:viam_chartplotter_mobile/boat_state.dart';

void main() {
  test('fresh boat fix takes precedence over the phone', () {
    final s = BoatState();
    s.update(position: const LatLng(41.0, -72.0)); // stamps lastBoatFixAt
    s.setPhonePosition(const LatLng(40.0, -73.0));
    expect(s.boatFixFresh, isTrue);
    expect(s.usingPhoneGps, isFalse);
    expect(s.displayPosition, const LatLng(41.0, -72.0));
  });

  test('stale boat fix falls back to the phone, labelled', () {
    final s = BoatState();
    s.update(position: const LatLng(41.0, -72.0));
    s.lastBoatFixAt = DateTime.now().subtract(const Duration(minutes: 2));
    s.setPhonePosition(const LatLng(40.0, -73.0));
    expect(s.boatFixFresh, isFalse);
    expect(s.usingPhoneGps, isTrue);
    expect(s.displayPosition, const LatLng(40.0, -73.0));
  });

  test('no phone fix and stale boat shows the last known boat position', () {
    final s = BoatState();
    s.update(position: const LatLng(41.0, -72.0));
    s.lastBoatFixAt = DateTime.now().subtract(const Duration(minutes: 2));
    expect(s.usingPhoneGps, isFalse);
    expect(s.displayPosition, const LatLng(41.0, -72.0));
  });

  test('boat GPS returning takes precedence again automatically', () {
    final s = BoatState();
    s.setPhonePosition(const LatLng(40.0, -73.0));
    expect(s.usingPhoneGps, isTrue);
    s.update(position: const LatLng(41.0, -72.0));
    expect(s.usingPhoneGps, isFalse);
    expect(s.displayPosition, const LatLng(41.0, -72.0));
  });

  test('phone fixes never enter the boat track', () {
    final s = BoatState();
    s.update(position: const LatLng(41.0, -72.0));
    final before = s.track.points.length;
    s.setPhonePosition(const LatLng(40.0, -73.0));
    s.setPhonePosition(const LatLng(40.1, -73.1));
    expect(s.track.points.length, before);
  });
}
