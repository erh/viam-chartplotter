import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import 'weather.dart';

/// Static wind arrows over the chart, sampled from a [WindField] on a screen-
/// adaptive grid and coloured by speed. Rebuilds on pan/zoom via MapCamera.of.
class WindLayer extends StatelessWidget {
  const WindLayer({super.key, required this.field});
  final WindField field;

  @override
  Widget build(BuildContext context) {
    return IgnorePointer(
      child: CustomPaint(
        size: Size.infinite,
        painter: _WindPainter(field, MapCamera.of(context)),
      ),
    );
  }
}

class _WindPainter extends CustomPainter {
  _WindPainter(this.field, this.camera);
  final WindField field;
  final MapCamera camera;

  @override
  void paint(Canvas canvas, Size size) {
    final b = camera.visibleBounds;
    final north = b.north, south = b.south;
    var west = b.west, east = b.east;
    if (east < west) east += 360; // simple antimeridian guard

    // ~16 arrows across the wider dimension, never finer than the data grid.
    final span = math.max(east - west, north - south);
    final step = math.max(field.dx, span / 16);
    if (step <= 0) return;

    final paint = Paint()
      ..strokeWidth = 2
      ..strokeCap = StrokeCap.round;

    for (double lat = north; lat >= south; lat -= step) {
      for (double lon = west; lon <= east; lon += step) {
        final nlon = ((lon + 540) % 360) - 180; // normalise to [-180,180)
        final s = field.sample(nlon, lat);
        if (s == null) continue;

        final pt = camera.latLngToScreenPoint(LatLng(lat, nlon));
        final cx = pt.x.toDouble(), cy = pt.y.toDouble();
        if (cx < -20 || cy < -20 || cx > size.width + 20 || cy > size.height + 20) {
          continue;
        }

        final knots = math.sqrt(s.u * s.u + s.v * s.v) * 1.94384;
        paint.color = _windColor(knots);

        // Screen vector points where the wind blows TO: +x east, +y south.
        final ang = math.atan2(-s.v, s.u);
        const len = 13.0;
        final dx = math.cos(ang) * len, dy = math.sin(ang) * len;
        final tail = Offset(cx - dx / 2, cy - dy / 2);
        final head = Offset(cx + dx / 2, cy + dy / 2);
        canvas.drawLine(tail, head, paint);
        // Arrowhead.
        const hl = 5.0;
        canvas.drawLine(
            head, head + Offset(math.cos(ang + 2.6) * hl, math.sin(ang + 2.6) * hl), paint);
        canvas.drawLine(
            head, head + Offset(math.cos(ang - 2.6) * hl, math.sin(ang - 2.6) * hl), paint);
      }
    }
  }

  Color _windColor(double kn) {
    if (kn < 5) return const Color(0xFFabd9e9);
    if (kn < 12) return const Color(0xFF74add1);
    if (kn < 18) return const Color(0xFF66bd63);
    if (kn < 25) return const Color(0xFFfdae61);
    if (kn < 34) return const Color(0xFFf46d43);
    return const Color(0xFFd73027);
  }

  @override
  bool shouldRepaint(covariant _WindPainter old) => true;
}
