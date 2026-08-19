import 'dart:async';

import 'package:geolocator/geolocator.dart';
import 'package:latlong2/latlong.dart';

import 'boat_state.dart';

/// Device-GPS fallback (L5) — a real advantage the web app can't have: when
/// the boat's GPS is unavailable (no movement sensor, dead sensor, dropped
/// connection) the phone in your hand still knows where it is.
///
/// The phone fix is never a silent substitute: it lands in
/// [BoatState.phonePosition] and the UI labels it unmistakably (amber dot +
/// "PHONE GPS" chip). Permission is requested lazily — only once the boat's
/// fix has actually gone stale — and refusal degrades cleanly to "no
/// position", permanently for this run (no permission nagging loop).
class PhoneGps {
  PhoneGps(this.state);

  final BoatState state;
  Timer? _watch;
  StreamSubscription<Position>? _sub;
  bool _refused = false;

  void start() {
    _watch ??= Timer.periodic(const Duration(seconds: 5), (_) => _evaluate());
  }

  Future<void> stop() async {
    _watch?.cancel();
    _watch = null;
    await _sub?.cancel();
    _sub = null;
    state.setPhonePosition(null);
  }

  Future<void> _evaluate() async {
    if (state.boatFixFresh) {
      // Boat GPS is back — it takes precedence automatically; drop the
      // phone stream (and its battery cost) until needed again.
      if (_sub != null) {
        await _sub?.cancel();
        _sub = null;
        state.setPhonePosition(null);
      }
      return;
    }
    if (_sub != null || _refused) return;
    try {
      var permission = await Geolocator.checkPermission();
      if (permission == LocationPermission.denied) {
        permission = await Geolocator.requestPermission();
      }
      if (permission == LocationPermission.denied ||
          permission == LocationPermission.deniedForever) {
        _refused = true; // works with no position; don't nag
        return;
      }
      _sub = Geolocator.getPositionStream(
        locationSettings: const LocationSettings(
          accuracy: LocationAccuracy.best,
          distanceFilter: 3,
        ),
      ).listen(
        (p) => state.setPhonePosition(LatLng(p.latitude, p.longitude)),
        onError: (_) {}, // location services off — stay positionless
      );
    } catch (_) {
      _refused = true;
    }
  }
}
