import 'dart:math' as math;
import 'dart:ui' as ui;

import 'package:flutter/material.dart';
import 'package:flutter/scheduler.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../weather.dart';

/// ol-wind-style particle animation — the web app's weather rendering,
/// ported so both apps show the same picture. Particles advect through the
/// U/V field in DEGREES (matching ol-wind under useGeographic), leave short
/// trails, and are coloured/sized by magnitude through the same scales and
/// formulas as src/lib/WeatherOverlays.svelte's layer configs.
class WeatherParticleConfig {
  const WeatherParticleConfig({
    required this.colorScale,
    required this.maxValue,
    required this.velocityFactor,
    required this.paths,
    required this.particleAge,
    required this.lineWidthFor,
    required this.alpha,
    required this.trailLen,
  });

  final List<Color> colorScale;
  final double maxValue; // colour-scale top (m/s wind, m waves)
  /// Degrees moved per frame per unit magnitude is velocityFactor / 2^zoom
  /// (web: velocityScale closures).
  final double velocityFactor;
  final int paths; // web particle count (desktop canvas)
  final int particleAge; // frames before re-randomising
  final double Function(double magnitude) lineWidthFor;
  final double alpha; // web globalAlpha: stroke brightness
  /// Trail length in frames. The web gets trails from canvas accumulation
  /// (retained fraction = globalAlpha); ~1/(1-alpha) frames of visible
  /// history is the equivalent here.
  final int trailLen;

  /// Wind: WeatherOverlays.svelte setupWindLayer.
  static const wind = WeatherParticleConfig(
    colorScale: windColorScale,
    maxValue: windRangeMaxMs,
    velocityFactor: 0.225,
    paths: 2500,
    particleAge: 100,
    lineWidthFor: _windWidth,
    alpha: 0.82,
    trailLen: 6,
  );

  /// Waves: slower drift, thicker/brighter strokes (setup at :651-680).
  static const waves = WeatherParticleConfig(
    colorScale: waveColorScale,
    maxValue: waveRangeMaxM,
    velocityFactor: 0.12,
    paths: 6000,
    particleAge: 100,
    lineWidthFor: _waveWidth,
    alpha: 0.97,
    trailLen: 20,
  );

  static double _windWidth(double m) => 2.7 + math.max(0.0, m) * 0.11;
  static double _waveWidth(double m) => 7.5;
}

class _Particle {
  double lng = 0, lat = 0;
  double magnitude = 0;
  int age = 0;
  // Geo-anchored trail (flat lng,lat pairs) so pan/zoom doesn't smear it.
  final List<double> trail = [];
  bool dead = true;
}

class WeatherParticleLayer extends StatefulWidget {
  const WeatherParticleLayer(
      {super.key, required this.field, required this.config});

  final WindField field;
  final WeatherParticleConfig config;

  @override
  State<WeatherParticleLayer> createState() => _WeatherParticleLayerState();
}

class _WeatherParticleLayerState extends State<WeatherParticleLayer>
    with SingleTickerProviderStateMixin {
  late final Ticker _ticker = createTicker(_tick);
  final math.Random _rng = math.Random();
  List<_Particle> _particles = const [];
  Duration _last = Duration.zero;
  int _repaint = 0;

  @override
  void initState() {
    super.initState();
    _ticker.start();
  }

  @override
  void dispose() {
    _ticker.dispose();
    super.dispose();
  }

  /// The web's counts are tuned for a desktop canvas; scale by screen area
  /// so a phone gets a similar visual density (and frame budget).
  int _targetCount(Size screen) {
    final scale = (screen.width * screen.height) / (1440.0 * 900.0);
    return (widget.config.paths * scale)
        .round()
        .clamp(300, widget.config.paths);
  }

  void _respawn(_Particle p, LatLngBounds b) {
    final latSpan = b.north - b.south;
    final lngSpan = b.east - b.west;
    p.lat = b.south - latSpan * 0.1 + _rng.nextDouble() * latSpan * 1.2;
    p.lng = b.west - lngSpan * 0.1 + _rng.nextDouble() * lngSpan * 1.2;
    p.age = _rng.nextInt(widget.config.particleAge); // desynchronised resets
    p.trail.clear();
    p.dead = false;
  }

  void _tick(Duration elapsed) {
    if (!mounted) return;
    final camera = MapCamera.of(context);
    final bounds = camera.visibleBounds;
    // Frame-rate independent: the web advects per ~60 fps frame.
    final frames =
        _last == Duration.zero ? 1.0 : (elapsed - _last).inMicroseconds / 16667.0;
    _last = elapsed;
    final step = frames.clamp(0.25, 4.0);

    final want = _targetCount(MediaQuery.sizeOf(context));
    if (_particles.length != want) {
      _particles = List.generate(want, (_) => _Particle());
    }

    final cfg = widget.config;
    final vel = cfg.velocityFactor / math.pow(2, camera.zoom) * step;
    for (final p in _particles) {
      if (p.dead) _respawn(p, bounds);
      final s = widget.field.sampleInterp(p.lng, p.lat);
      p.age += step.ceil();
      if (s == null ||
          p.age > cfg.particleAge ||
          p.lat < bounds.south - (bounds.north - bounds.south) * 0.15 ||
          p.lat > bounds.north + (bounds.north - bounds.south) * 0.15 ||
          p.lng < bounds.west - (bounds.east - bounds.west) * 0.15 ||
          p.lng > bounds.east + (bounds.east - bounds.west) * 0.15) {
        p.dead = true;
        continue;
      }
      p.magnitude = math.sqrt(s.u * s.u + s.v * s.v);
      p.trail..add(p.lng)..add(p.lat);
      if (p.trail.length > cfg.trailLen * 2) {
        p.trail.removeRange(0, p.trail.length - cfg.trailLen * 2);
      }
      p.lng += s.u * vel;
      p.lat += s.v * vel;
    }
    setState(() => _repaint++);
  }

  @override
  Widget build(BuildContext context) {
    final camera = MapCamera.of(context);
    return IgnorePointer(
      child: RepaintBoundary(
        child: CustomPaint(
          size: Size.infinite,
          painter: _ParticlePainter(
            particles: _particles,
            config: widget.config,
            camera: camera,
            repaint: _repaint,
          ),
        ),
      ),
    );
  }
}

class _ParticlePainter extends CustomPainter {
  _ParticlePainter({
    required this.particles,
    required this.config,
    required this.camera,
    required this.repaint,
  });

  final List<_Particle> particles;
  final WeatherParticleConfig config;
  final MapCamera camera;
  final int repaint;

  @override
  void paint(Canvas canvas, Size size) {
    final paint = Paint()
      ..style = PaintingStyle.stroke
      ..strokeCap = StrokeCap.round;
    for (final p in particles) {
      if (p.dead || p.trail.length < 4) continue;
      final pts = <Offset>[];
      for (var i = 0; i < p.trail.length; i += 2) {
        final sp =
            camera.latLngToScreenPoint(LatLng(p.trail[i + 1], p.trail[i]));
        pts.add(Offset(sp.x, sp.y));
      }
      paint
        ..color = colorForValue(config.colorScale, p.magnitude, config.maxValue)
            .withValues(alpha: config.alpha)
        ..strokeWidth = config.lineWidthFor(p.magnitude);
      canvas.drawPoints(ui.PointMode.polygon, pts, paint);
    }
  }

  @override
  bool shouldRepaint(_ParticlePainter old) => true;
}
