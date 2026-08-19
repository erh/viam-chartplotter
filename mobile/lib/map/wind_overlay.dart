import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../boat_state.dart';
import '../app_config.dart';
import '../settings.dart';
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

  /// Arrow cap, mirroring the same protection the AIS layer needs (task D10).
  static const int _maxMarkers = 1500;

  WindField? field;
  bool on = false;
  bool loading = false;
  int fh = 0; // forecast hour (0 = latest analysis)

  // Model catalogue (F2): fetched once per session from
  // /noaa-weather/models; the GFS fallback keeps older servers working.
  // The slider's range and step come from [model], never from constants.
  List<WeatherModel> models = const [WeatherModel.gfsFallback];
  WeatherModel model = WeatherModel.gfsFallback;
  bool _modelsFetched = false;

  /// Wind-kind entries for the picker (disabled ones included — they show
  /// greyed with their reason, matching the web's tooltip).
  List<WeatherModel> get windModels =>
      [for (final m in models) if (m.kind == 'wind') m];

  /// Fetch the catalogue once and re-resolve the persisted selection
  /// against it. Never throws — a failure keeps the GFS defaults.
  Future<void> ensureModels() async {
    if (_modelsFetched) return;
    _modelsFetched = true;
    models = await fetchWeatherModels(AppConfig.tileBase.value);
    final wanted = Settings.instance.windModel;
    model = windModels.firstWhere(
      (m) => m.name == wanted && !m.disabled,
      orElse: () => windModels.firstWhere((m) => !m.disabled,
          orElse: () => WeatherModel.gfsFallback),
    );
    fh = model.clampFh(fh);
  }

  /// Switch models (F2): persist, reshape the forecast hour into the new
  /// model's range, and refetch. Throws like [load] so the caller can show
  /// why (the selection is rolled back — the UI must not claim a model that
  /// isn't rendering, the web has the same rule).
  Future<void> selectModel(WeatherModel next) async {
    if (next.name == model.name || next.disabled) return;
    final prev = model;
    model = next;
    fh = next.clampFh(fh);
    Settings.instance.windModel = next.name;
    try {
      await load(fh);
    } catch (_) {
      model = prev;
      fh = prev.clampFh(fh);
      Settings.instance.windModel = prev.name;
      rethrow;
    }
  }

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
    await ensureModels();
    await load(fh);
  }

  /// Fetch the wind field at forecast hour [hour] and show it.
  Future<void> load(int hour) async {
    loading = true;
    final h = model.clampFh(hour);
    state.setWindInfo('fetching ${model.name} fh=$h …');
    try {
      final f =
          await fetchWindField(AppConfig.tileBase.value, model.name, fh: h);
      field = f;
      on = true;
      fh = h;
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
/// [onScrub] previews a value while dragging; [onCommit] fetches it. The
/// range and step come from the selected [model] (F2); with more than one
/// wind model available a picker sits beside the slider.
class WindForecastBar extends StatelessWidget {
  const WindForecastBar({
    super.key,
    required this.fh,
    required this.loading,
    required this.onScrub,
    required this.onCommit,
    this.model = WeatherModel.gfsFallback,
    this.models = const [],
    this.onModel,
  });

  final int fh;
  final bool loading;
  final ValueChanged<int> onScrub;
  final ValueChanged<int> onCommit;
  final WeatherModel model;
  final List<WeatherModel> models;
  final ValueChanged<WeatherModel>? onModel;

  @override
  Widget build(BuildContext context) {
    final divisions =
        ((model.maxFh - model.minFh) / (model.stepFh > 0 ? model.stepFh : 3))
            .round();
    int snap(double v) => model.clampFh(v.round());
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
          // Model picker (F2), only when there's a real choice (web parity).
          if (models.length > 1)
            Padding(
              padding: const EdgeInsets.only(left: 6),
              child: DropdownButton<String>(
                value: model.name,
                dropdownColor: Colors.black87,
                underline: const SizedBox.shrink(),
                isDense: true,
                style: const TextStyle(color: Colors.white, fontSize: 13),
                items: [
                  for (final m in models)
                    DropdownMenuItem(
                      value: m.name,
                      enabled: !m.disabled,
                      child: Column(
                        mainAxisSize: MainAxisSize.min,
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text(
                            m.disabled ? '${m.displayName} (n/a)' : m.displayName,
                            style: TextStyle(
                                color: m.disabled
                                    ? Colors.white38
                                    : Colors.white,
                                fontSize: 13),
                          ),
                          // The web puts `reason` in the option tooltip;
                          // touch has no hover, so it's a subtitle here.
                          if (m.disabled && (m.reason ?? '').isNotEmpty)
                            Text(m.reason!,
                                style: const TextStyle(
                                    color: Colors.white38, fontSize: 10)),
                        ],
                      ),
                    ),
                ],
                selectedItemBuilder: (_) => [
                  for (final m in models)
                    Center(
                        child: Text(m.displayName,
                            style: const TextStyle(
                                color: Colors.white, fontSize: 13))),
                ],
                onChanged: loading || onModel == null
                    ? null
                    : (name) {
                        final m = models.where((x) => x.name == name);
                        if (m.isNotEmpty) onModel!(m.first);
                      },
              ),
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
              value: fh.toDouble().clamp(
                  model.minFh.toDouble(), model.maxFh.toDouble()),
              min: model.minFh.toDouble(),
              max: model.maxFh.toDouble(),
              divisions: divisions > 0 ? divisions : null,
              label: fh == 0 ? 'now' : '+${fh}h',
              onChanged: (v) => onScrub(snap(v)),
              onChangeEnd: (v) => onCommit(snap(v)),
            ),
          ),
        ],
      ),
    );
  }
}
