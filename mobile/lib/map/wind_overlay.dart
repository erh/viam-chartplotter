import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../boat_state.dart';
import '../app_config.dart';
import '../weather.dart';

/// Owns the wind overlay: the fetched field, the on/off + forecast-hour state,
/// and the arrow markers sampled for the current viewport.
///
/// Kept out of MapScreen so the weather tasks (F2 model picker, F3 waves,
/// F4 isobars, F7 point sample, F8 zoom gates) land in one file rather than
/// in the middle of the map screen.
///
/// The screen owns the widget lifecycle: it sets [bounds] / [rotationDeg] from
/// the map camera and calls [rebuildMarkers] from event handlers — never from
/// build.
class WindOverlayController {
  WindOverlayController({required this.state});

  final BoatState state;

  /// GFS: 0..240 h in 3 h steps. Task F2 replaces this constant with the
  /// selected model's own range from /noaa-weather/models.
  static const int maxFh = 240;

  /// Arrow cap, mirroring the same protection the AIS layer needs (task D10).
  static const int _maxMarkers = 1500;

  WindField? field;
  bool on = false;
  bool loading = false;
  int fh = 0; // forecast hour (0 = latest analysis)

  /// Current viewport and chart rotation, mirrored from the map camera by the
  /// screen so marker sampling and arrow angles stay in sync with the chart.
  LatLngBounds? bounds;
  double rotationDeg = 0;

  List<Marker> markers = const [];

  /// Turn the overlay on/off, fetching the field on first use. Throws whatever
  /// the fetch threw so the caller can surface it; [state]'s wind-info row is
  /// updated either way.
  Future<void> toggle() async {
    if (on) {
      on = false;
      return;
    }
    if (field != null) {
      on = true;
      rebuildMarkers();
      return;
    }
    await load(fh);
  }

  /// Fetch the wind field at forecast hour [hour] and show it.
  Future<void> load(int hour) async {
    loading = true;
    state.setWindInfo('fetching gfs fh=$hour …');
    try {
      final f = await fetchWindField(AppConfig.tileBase.value, 'gfs', fh: hour);
      field = f;
      on = true;
      fh = hour;
      loading = false;
      rebuildMarkers(); // also updates the Debug wind row with the count
    } catch (e) {
      loading = false;
      state.setWindInfo('error: $e');
      rethrow;
    }
  }

  /// Rebuild the arrow markers for the current field + viewport, caching them
  /// and reporting the count into the Debug wind row. Uses MarkerLayer (proven
  /// to render) rather than a CustomPainter.
  void rebuildMarkers() {
    final f = field;
    final b = bounds;
    if (!on || f == null || b == null) {
      markers = const [];
      return;
    }
    final out = <Marker>[];
    final span = math.max(b.east - b.west, b.north - b.south);
    // ~12 arrows across the view at any zoom (not tied to the 0.25° grid).
    final step = (span / 12).clamp(0.02, 5.0).toDouble();
    if (step > 0) {
      // Snap the lattice to multiples of `step` so arrows stay anchored to the
      // ground and scroll with the map when panning (instead of the viewport).
      final lat0 = (b.south / step).floorToDouble() * step;
      final lon0 = (b.west / step).floorToDouble() * step;
      for (double lat = lat0; lat <= b.north; lat += step) {
        for (double lon = lon0; lon <= b.east; lon += step) {
          final nlon = ((lon + 540) % 360) - 180;
          final s = f.sampleInterp(nlon, lat);
          if (s == null) continue;
          final knots = math.sqrt(s.u * s.u + s.v * s.v) * 1.94384;
          // bearing wind blows toward, offset by the chart rotation so arrows
          // stay aligned in course-up mode.
          final ang = math.atan2(s.u, s.v) + rotationDeg * math.pi / 180.0;
          out.add(Marker(
            point: LatLng(lat, nlon),
            width: 24,
            height: 24,
            child: Transform.rotate(
              angle: ang,
              child: Icon(Icons.navigation, size: 16, color: colorFor(knots)),
            ),
          ));
          if (out.length >= _maxMarkers) break;
        }
        if (out.length >= _maxMarkers) break;
      }
    }
    markers = out;
    state.setWindInfo(
      'loaded ${f.nx}×${f.ny}; ${out.length} arrows; '
      'view ${b.south.toStringAsFixed(1)}..${b.north.toStringAsFixed(1)}N, '
      '${b.west.toStringAsFixed(1)}..${b.east.toStringAsFixed(1)}E; step '
      '${step.toStringAsFixed(2)}°',
    );
  }

  /// Wind-speed colour ramp (knots). Shared with the legend when task F3 adds
  /// one.
  static Color colorFor(double kn) {
    if (kn < 5) return const Color(0xFFabd9e9);
    if (kn < 12) return const Color(0xFF74add1);
    if (kn < 18) return const Color(0xFF66bd63);
    if (kn < 25) return const Color(0xFFfdae61);
    if (kn < 34) return const Color(0xFFf46d43);
    return const Color(0xFFd73027);
  }
}

/// Bottom forecast-time slider, shown only while the wind overlay is on.
/// [onScrub] previews a value while dragging; [onCommit] fetches it.
class WindForecastBar extends StatelessWidget {
  const WindForecastBar({
    super.key,
    required this.fh,
    required this.loading,
    required this.onScrub,
    required this.onCommit,
  });

  final int fh;
  final bool loading;
  final ValueChanged<int> onScrub;
  final ValueChanged<int> onCommit;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 2),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(24),
      ),
      child: Row(
        children: [
          SizedBox(
            width: 18,
            height: 18,
            child: loading
                ? const CircularProgressIndicator(
                    strokeWidth: 2, color: Colors.white)
                : const Icon(Icons.air, color: Colors.white, size: 18),
          ),
          SizedBox(
            width: 48,
            child: Text(
              fh == 0 ? 'now' : '+${fh}h',
              textAlign: TextAlign.center,
              style: const TextStyle(color: Colors.white, fontSize: 13),
            ),
          ),
          Expanded(
            child: Slider(
              value: fh.toDouble(),
              max: WindOverlayController.maxFh.toDouble(),
              divisions: WindOverlayController.maxFh ~/ 3,
              label: fh == 0 ? 'now' : '+${fh}h',
              onChanged: (v) => onScrub((v / 3).round() * 3),
              onChangeEnd: (v) => onCommit((v / 3).round() * 3),
            ),
          ),
        ],
      ),
    );
  }
}
