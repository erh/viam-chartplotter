import 'package:flutter/foundation.dart' show listEquals;
import 'package:flutter/material.dart';

import 'navaids.dart';

/// Navaid chart symbol (B1) — the CustomPainter equivalent of the web app's
/// synthesized SVGs (buoyBody/beaconBody, src/marineMap.svelte:1624-1760).
/// Same 24×24 canvas with the structure's footprint at (12, 18); the
/// upper-right quadrant is reserved for the magenta light flare when the
/// structure is co-located with a LIGHTS feature.
///
/// The web caches data-URLs by (class|shape|colours|lighted); here the
/// painter itself is cheap, and Flutter raster-caches repaint-boundary
/// widgets, so the same key-space of a few dozen combinations repaints only
/// when it first appears.
class NavaidIcon extends StatelessWidget {
  const NavaidIcon({super.key, required this.feature});
  final NavaidFeature feature;

  @override
  Widget build(BuildContext context) {
    return RepaintBoundary(
      child: CustomPaint(
        size: const Size(24, 24),
        painter: _NavaidPainter(
          class_: feature.class_,
          shape: feature.shape,
          colours: navaidColours(feature.props),
          lighted: feature.lighted,
        ),
      ),
    );
  }
}

class _NavaidPainter extends CustomPainter {
  _NavaidPainter({
    required this.class_,
    required this.shape,
    required this.colours,
    required this.lighted,
  });

  final String class_;
  final int shape;
  final List<Color> colours;
  final bool lighted;

  static const double ax = 12; // anchor x — the footprint pixel
  static const double ay = 18; // anchor y

  Color get c1 => colours.first;
  Color get c2 => colours.length > 1 ? colours[1] : colours.first;

  @override
  void paint(Canvas canvas, Size size) {
    final stroke = Paint()
      ..color = Colors.black
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1;

    if (class_ == 'LIGHTS') {
      // Standalone light: magenta starburst (grey default → magenta).
      final c = c1 == navaidGrey ? navaidLightMagenta : c1;
      final star = Path()
        ..moveTo(ax, ay - 8)
        ..lineTo(ax + 1.2, ay - 1.5)
        ..lineTo(ax + 7, ay)
        ..lineTo(ax + 1.2, ay + 1.5)
        ..lineTo(ax, ay + 7)
        ..lineTo(ax - 1.2, ay + 1.5)
        ..lineTo(ax - 7, ay)
        ..lineTo(ax - 1.2, ay - 1.5)
        ..close();
      canvas.drawPath(star, Paint()..color = c);
      canvas.drawPath(star, stroke..strokeWidth = 0.6);
      return;
    }

    if (class_.startsWith('BCN')) {
      _bandedRect(canvas, w: 4, h: 13, stroke: stroke);
      canvas.drawCircle(
          const Offset(ax, ay), 1, Paint()..color = Colors.black);
    } else if (class_ == 'DAYMAR') {
      final diamond = Path()
        ..moveTo(ax, ay - 7)
        ..lineTo(ax + 5, ay - 2)
        ..lineTo(ax, ay + 3)
        ..lineTo(ax - 5, ay - 2)
        ..close();
      canvas.drawPath(diamond, Paint()..color = c1);
      canvas.drawPath(diamond, stroke);
    } else {
      _buoyBody(canvas, stroke);
    }

    if (lighted) {
      // Magenta wedge flag up-and-right — S-52's lighted marker.
      final flare = Path()
        ..moveTo(ax - 1, ay - 9)
        ..lineTo(ax + 9, ay - 12)
        ..lineTo(ax + 1, ay - 6)
        ..close();
      canvas.drawPath(flare, Paint()..color = navaidLightMagenta);
      canvas.drawPath(flare, stroke..strokeWidth = 0.5);
    }
  }

  /// Two-band vertical structure outlined in black (beacon, can, pillar,
  /// spar bodies share this with different proportions).
  void _bandedRect(Canvas canvas,
      {required double w, required double h, required Paint stroke}) {
    final rect = Rect.fromLTWH(ax - w / 2, ay - h, w, h);
    canvas.save();
    canvas.clipRect(rect);
    canvas.drawRect(
        Rect.fromLTWH(ax - w / 2, ay - h, w, h / 2), Paint()..color = c1);
    canvas.drawRect(
        Rect.fromLTWH(ax - w / 2, ay - h / 2, w, h / 2), Paint()..color = c2);
    canvas.restore();
    canvas.drawRect(rect, stroke);
  }

  /// Two-band circle split vertically (spherical/super-buoy).
  void _bandedCircle(Canvas canvas, double r, Paint stroke) {
    final circle = Path()
      ..addOval(Rect.fromCircle(center: Offset(ax, ay - r), radius: r));
    canvas.save();
    canvas.clipPath(circle);
    canvas.drawRect(
        Rect.fromLTWH(ax - r, ay - 2 * r, r, 2 * r), Paint()..color = c1);
    canvas.drawRect(
        Rect.fromLTWH(ax, ay - 2 * r, r, 2 * r), Paint()..color = c2);
    canvas.restore();
    canvas.drawPath(circle, stroke);
  }

  void _buoyBody(Canvas canvas, Paint stroke) {
    switch (shape) {
      case 1: // Conical, point up.
        const h = 11.0, w = 8.0;
        final cone = Path()
          ..moveTo(ax, ay - h)
          ..lineTo(ax + w / 2, ay)
          ..lineTo(ax - w / 2, ay)
          ..close();
        canvas.save();
        canvas.clipPath(cone);
        canvas.drawRect(const Rect.fromLTWH(ax - w / 2, ay - h, w, h / 2),
            Paint()..color = c1);
        canvas.drawRect(const Rect.fromLTWH(ax - w / 2, ay - h / 2, w, h / 2),
            Paint()..color = c2);
        canvas.restore();
        canvas.drawPath(cone, stroke);
      case 2: // Can / cylindrical.
        _bandedRect(canvas, w: 8, h: 9, stroke: stroke);
      case 3: // Spherical.
        _bandedCircle(canvas, 5, stroke);
      case 4: // Pillar.
        _bandedRect(canvas, w: 6, h: 13, stroke: stroke);
      case 5: // Spar.
        _bandedRect(canvas, w: 3, h: 14, stroke: stroke);
      case 6: // Barrel.
        final barrel = Rect.fromCenter(
            center: const Offset(ax, ay - 4), width: 12, height: 8);
        canvas.drawOval(barrel, Paint()..color = c1);
        canvas.drawOval(barrel, stroke);
      case 7: // Super-buoy.
        _bandedCircle(canvas, 7, stroke);
      default: // Unknown shape — generic dot keeps the chart usable.
        canvas.drawCircle(const Offset(ax, ay - 4), 4, Paint()..color = c1);
        canvas.drawCircle(const Offset(ax, ay - 4), 4, stroke);
    }
  }

  @override
  bool shouldRepaint(covariant _NavaidPainter old) =>
      old.class_ != class_ ||
      old.shape != shape ||
      old.lighted != lighted ||
      !listEquals(old.colours, colours);
}
