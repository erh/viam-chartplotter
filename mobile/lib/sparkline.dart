import 'dart:math' show max, min;

import 'package:flutter/material.dart';

/// Tiny trend line for a rolling series of values (auto-scaled to its own
/// min/max). Used next to the drawer readouts. Renders nothing until there are
/// at least 2 points.
class Sparkline extends StatelessWidget {
  const Sparkline({
    super.key,
    required this.data,
    this.color = Colors.white70,
    this.width = 60,
    this.height = 20,
  });

  final List<double> data;
  final Color color;
  final double width;
  final double height;

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      width: width,
      height: height,
      child: (data.length < 2)
          ? null
          : CustomPaint(painter: _SparkPainter(data, color)),
    );
  }
}

class _SparkPainter extends CustomPainter {
  _SparkPainter(this.data, this.color);
  final List<double> data;
  final Color color;

  @override
  void paint(Canvas canvas, Size size) {
    var lo = data.reduce(min);
    var hi = data.reduce(max);
    if (hi - lo < 1e-9) hi = lo + 1; // flat series → avoid divide-by-zero
    final path = Path();
    for (var i = 0; i < data.length; i++) {
      final x = i / (data.length - 1) * size.width;
      final y = size.height - (data[i] - lo) / (hi - lo) * size.height;
      if (i == 0) {
        path.moveTo(x, y);
      } else {
        path.lineTo(x, y);
      }
    }
    canvas.drawPath(
      path,
      Paint()
        ..color = color
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1.5
        ..strokeJoin = StrokeJoin.round,
    );
  }

  @override
  bool shouldRepaint(covariant _SparkPainter old) =>
      old.data != data || old.color != color;
}
