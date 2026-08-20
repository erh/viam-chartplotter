import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../boat_state.dart';
import '../app_config.dart';
import '../isobars.dart';
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

  /// Weather zoom gate (F8), web parity (LayerOption.maxZoom = 12): past
  /// this zoom one 0.25° model cell spans hundreds of screen pixels and the
  /// field is a meaningless wash, so it hides — with a hint, not silently.
  static const double weatherMaxZoom = 12;

  static bool weatherVisibleAtZoom(double zoom) => zoom <= weatherMaxZoom;

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

  List<WeatherModel> get waveModels =>
      [for (final m in models) if (m.kind == 'wave') m];

  // ---- wave overlay (F3) ------------------------------------------------
  // Waves ride the same 2-record U/V JSON as wind (the server re-stamps
  // HTSGW/DIRPW): magnitude = significant wave height in metres, direction
  // = propagation. Height drives the colour, exactly like the web.
  WindField? waveField;
  bool wavesOn = false;
  bool waveLoading = false;
  WeatherModel? waveModel;

  WeatherModel? _resolveWaveModel() {
    final wanted = Settings.instance.waveModel;
    final usable = [for (final m in waveModels) if (!m.disabled) m];
    if (usable.isEmpty) return null;
    return usable.firstWhere((m) => m.name == wanted, orElse: () => usable.first);
  }

  /// Toggle the wave overlay; fetches the field on first use. Throws like
  /// [toggle] so the caller can surface why.
  Future<void> toggleWaves() async {
    if (wavesOn) {
      wavesOn = false;
      Settings.instance.wavesOn = false;
      return;
    }
    await ensureModels();
    final m = _resolveWaveModel();
    if (m == null) throw StateError('No wave model available on this server');
    waveModel = m;
    Settings.instance.wavesOn = true;
    if (waveField != null) {
      wavesOn = true;
      rebuildMarkers();
      return;
    }
    await loadWaves(fh);
  }

  Future<void> selectWaveModel(WeatherModel next) async {
    if (next.disabled || next.name == waveModel?.name) return;
    final prev = waveModel;
    waveModel = next;
    Settings.instance.waveModel = next.name;
    try {
      await loadWaves(fh);
    } catch (_) {
      waveModel = prev;
      Settings.instance.waveModel = prev?.name;
      rethrow;
    }
  }

  /// Fetch the wave field at forecast hour [hour] (clamped to the wave
  /// model's own range) and show it.
  Future<void> loadWaves(int hour) async {
    final m = waveModel;
    if (m == null) return;
    waveLoading = true;
    try {
      final f = await fetchWindField(AppConfig.tileBase.value, m.name,
          fh: m.clampFh(hour));
      waveField = f;
      wavesOn = true;
      waveLoading = false;
      rebuildMarkers();
    } catch (e) {
      waveLoading = false;
      state.setWindInfo('wave error: $e');
      rethrow;
    }
  }

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

  /// Current view zoom, mirrored like [bounds]. The isobar ladder (F4)
  /// thinks in the web's "resolution" (degrees of longitude per pixel);
  /// this converts so the thresholds stay byte-for-byte comparable.
  double viewZoom = 9;
  double get _resolutionDeg => 360 / (256 * math.pow(2, viewZoom));

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

  /// Rebuild the viewport-derived overlays. Wind/waves render as live
  /// particle layers (weather_particles.dart, matching the web's ol-wind)
  /// and need nothing built here — only the isobar geometry and the Debug
  /// wind row remain.
  void rebuildMarkers() {
    _rebuildIsobars();
    final f = field;
    if (on && f != null) {
      state.setWindInfo('loaded ${model.name} ${f.nx}×${f.ny} fh=$fh');
    }
  }

  // ---- isobars (F4) -----------------------------------------------------
  // The server runs marching squares over PRMSL and serves GeoJSON contour
  // segments; the client just draws them with the web's zoom/label ladder.
  IsobarField? isobarField;
  bool isobarsOn = false;
  bool isobarLoading = false;
  List<Polyline> isobarLines = const [];
  List<Marker> isobarMarkers = const []; // labels + H/L extrema

  String get _isobarModelName {
    final m = [
      for (final m in models)
        if (m.kind == 'isobars' && !m.disabled) m
    ];
    return m.isEmpty ? 'gfs-isobars' : m.first.name;
  }

  Future<void> toggleIsobars() async {
    if (isobarsOn) {
      isobarsOn = false;
      Settings.instance.isobarsOn = false;
      isobarLines = const [];
      isobarMarkers = const [];
      return;
    }
    await ensureModels();
    Settings.instance.isobarsOn = true;
    if (isobarField != null) {
      isobarsOn = true;
      rebuildMarkers();
      return;
    }
    await loadIsobars(fh);
  }

  Future<void> loadIsobars(int hour) async {
    isobarLoading = true;
    try {
      isobarField = await fetchIsobars(AppConfig.tileBase.value,
          model: _isobarModelName, fh: model.clampFh(hour));
      isobarsOn = true;
      isobarLoading = false;
      rebuildMarkers();
    } catch (e) {
      isobarLoading = false;
      state.setWindInfo('isobar error: $e');
      rethrow;
    }
  }

  /// Rebuild the isobar polylines + label/extremum markers for the current
  /// viewport, applying the ported ladder: 2 hPa fill lines drop at
  /// overview zoom, stroke weight by tier, labels sparse on a fixed world
  /// lattice so they don't crawl when panning.
  void _rebuildIsobars() {
    final f = isobarField;
    final b = bounds;
    if (!isobarsOn || f == null || b == null) {
      isobarLines = const [];
      isobarMarkers = const [];
      return;
    }
    final res = _resolutionDeg;
    final latM = (b.north - b.south) * 0.2;
    final lonM = (b.east - b.west) * 0.2;
    bool inView(LatLng p) =>
        p.latitude >= b.south - latM &&
        p.latitude <= b.north + latM &&
        p.longitude >= b.west - lonM &&
        p.longitude <= b.east + lonM;

    const maxSegments = 4000;
    final lines = <Polyline>[];
    final markers = <Marker>[];
    for (final line in f.lines) {
      if (lines.length >= maxSegments) break;
      if (!isobarLineVisible(line.hPa, res)) continue;
      if (!inView(line.labelAnchor)) continue;
      final double width;
      switch (isobarTier(line.hPa)) {
        case IsobarTier.reference:
          width = 2.0;
        case IsobarTier.heavy:
          width = 1.4;
        case IsobarTier.standard:
          width = 1.0;
        case IsobarTier.half:
          width = 0.6;
      }
      lines.add(Polyline(
        points: line.points,
        color: Colors.black.withValues(alpha: 0.65),
        strokeWidth: width,
      ));
      final label = isobarLabel(line, res);
      if (label != null) {
        markers.add(Marker(
          point: line.labelAnchor,
          width: 40,
          height: 18,
          child: Container(
            alignment: Alignment.center,
            decoration: BoxDecoration(
              color: Colors.white.withValues(alpha: 0.75),
              borderRadius: BorderRadius.circular(4),
            ),
            child: Text(label,
                style: const TextStyle(
                    color: Colors.black,
                    fontSize: 10,
                    fontWeight: FontWeight.bold)),
          ),
        ));
      }
    }
    // H/L extrema: red highs, blue lows — surface-analysis convention.
    for (final e in f.extrema) {
      if (!inView(e.position)) continue;
      markers.add(Marker(
        point: e.position,
        width: 34,
        height: 34,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(e.kind,
                style: TextStyle(
                    color: e.kind == 'H' ? Colors.red : Colors.blue,
                    fontSize: 18,
                    fontWeight: FontWeight.w900)),
            Text('${e.hPa}',
                style: const TextStyle(color: Colors.black87, fontSize: 9)),
          ],
        ),
      ));
    }
    isobarLines = lines;
    isobarMarkers = markers;
  }

  /// Point weather sample (F7) — the web cursorInfo's numbers for a
  /// lon/lat, from whichever fields are loaded. Directions are
  /// meteorological ("from", like the web readout): atan2(u,v) is the
  /// direction of motion, +180 flips it to origin.
  ({double? windKt, double? windFromDeg, double? waveM, double? waveFromDeg})
      samplePoint(double lon, double lat) {
    double? windKt, windFrom, waveM, waveFrom;
    final w = on ? field?.sampleInterp(lon, lat) : null;
    if (w != null) {
      windKt = math.sqrt(w.u * w.u + w.v * w.v) * 1.94384;
      windFrom = (math.atan2(w.u, w.v) * 180 / math.pi + 180) % 360;
    }
    final s = wavesOn ? waveField?.sampleInterp(lon, lat) : null;
    if (s != null) {
      waveM = math.sqrt(s.u * s.u + s.v * s.v);
      waveFrom = (math.atan2(s.u, s.v) * 180 / math.pi + 180) % 360;
    }
    return (
      windKt: windKt,
      windFromDeg: windFrom,
      waveM: waveM,
      waveFromDeg: waveFrom
    );
  }

}

/// Callout lines for a point weather sample (F7), formatted like the web's
/// cursor readout: `4.20 nm @ 130°`, `wind 12 kt from 245°`,
/// `wave 3.2 ft from 180°`. Null inputs drop their line.
List<String> weatherSampleLines({
  double? rangeNm,
  double? bearingDeg,
  double? windKt,
  double? windFromDeg,
  double? waveM,
  double? waveFromDeg,
}) {
  String deg(double d) => d.round().toString().padLeft(3, '0');
  return [
    if (rangeNm != null && bearingDeg != null)
      '${rangeNm.toStringAsFixed(2)} nm @ ${deg(bearingDeg)}°',
    if (windKt != null && windFromDeg != null)
      'wind ${windKt.round()} kt from ${deg(windFromDeg)}°',
    if (waveM != null && waveFromDeg != null)
      'wave ${(waveM * metersToFeet).toStringAsFixed(1)} ft '
          'from ${deg(waveFromDeg)}°',
  ];
}

/// Compact model dropdown used by the forecast bar (F2 wind, F3 wave).
/// Disabled entries stay listed, greyed, with their `reason` as a subtitle
/// (the web puts it in the option tooltip; touch has no hover).
class _ModelPicker extends StatelessWidget {
  const _ModelPicker({
    required this.current,
    required this.models,
    required this.enabled,
    required this.onPick,
  });

  final WeatherModel current;
  final List<WeatherModel> models;
  final bool enabled;
  final ValueChanged<WeatherModel> onPick;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(left: 6),
      child: DropdownButton<String>(
        value: current.name,
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
                        color: m.disabled ? Colors.white38 : Colors.white,
                        fontSize: 13),
                  ),
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
                    style:
                        const TextStyle(color: Colors.white, fontSize: 13))),
        ],
        onChanged: !enabled
            ? null
            : (name) {
                final m = models.where((x) => x.name == name);
                if (m.isNotEmpty) onPick(m.first);
              },
      ),
    );
  }
}

/// Wave-height legend (F3): the colour strip with tick labels in FEET
/// (web: WAVE_RANGE_MAX_M ticks at 0/25/50/75/100% × METERS_TO_FEET).
class WaveLegend extends StatelessWidget {
  const WaveLegend({super.key});

  @override
  Widget build(BuildContext context) {
    final ticks = [
      for (final f in [0.0, 0.25, 0.5, 0.75, 1.0])
        '${(waveRangeMaxM * f * metersToFeet).round()} ft'
    ];
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
      decoration: BoxDecoration(
        color: Colors.black.withValues(alpha: 0.6),
        borderRadius: BorderRadius.circular(10),
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Container(
            height: 8,
            width: 180,
            decoration: BoxDecoration(
              borderRadius: BorderRadius.circular(4),
              gradient: const LinearGradient(colors: waveColorScale),
            ),
          ),
          const SizedBox(height: 2),
          SizedBox(
            width: 180,
            child: Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                for (final t in ticks)
                  Text(t,
                      style:
                          const TextStyle(color: Colors.white, fontSize: 10)),
              ],
            ),
          ),
        ],
      ),
    );
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
    this.waveModel,
    this.waveModels = const [],
    this.onWaveModel,
    this.playing = false,
    this.onPlay,
  });

  final int fh;
  final bool loading;
  final ValueChanged<int> onScrub;
  final ValueChanged<int> onCommit;
  final WeatherModel model;
  final List<WeatherModel> models;
  final ValueChanged<WeatherModel>? onModel;
  // Wave model picker (F3), shown when a wave overlay is up and there's a
  // choice. Null waveModel = waves off.
  final WeatherModel? waveModel;
  final List<WeatherModel> waveModels;
  final ValueChanged<WeatherModel>? onWaveModel;

  /// Forecast time-lapse: play steps the hour forward on a timer and loops.
  final bool playing;
  final VoidCallback? onPlay;

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
          // Forecast time-lapse (windy-style): step the next 24 h and loop.
          if (onPlay != null)
            IconButton(
              visualDensity: VisualDensity.compact,
              icon: Icon(
                playing ? Icons.pause_circle : Icons.play_circle,
                color: Colors.white,
                size: 22,
              ),
              tooltip: playing ? 'Pause' : 'Play next 24 h',
              onPressed: onPlay,
            ),
          // Model pickers (F2 wind / F3 wave), only when there's a real
          // choice (web parity).
          if (models.length > 1 && onModel != null)
            _ModelPicker(
                current: model, models: models,
                enabled: !loading, onPick: onModel!),
          if (waveModel != null && waveModels.length > 1 && onWaveModel != null)
            _ModelPicker(
                current: waveModel!, models: waveModels,
                enabled: !loading, onPick: onWaveModel!),
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
