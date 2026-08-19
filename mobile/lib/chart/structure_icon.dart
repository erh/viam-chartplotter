import 'package:flutter/material.dart';

/// Structure badge (B2) — the CustomPainter port of the web's
/// structureIconSrc: a 24×24 white circle badge with a class glyph
/// (stylised bridge arches, or an overhead line with a vertical-clearance
/// hint for cables/pipes/conveyors).
class StructureIcon extends StatelessWidget {
  const StructureIcon({super.key, required this.class_});
  final String class_;

  @override
  Widget build(BuildContext context) {
    return RepaintBoundary(
      child: CustomPaint(
        size: const Size(24, 24),
        painter: _StructurePainter(class_),
      ),
    );
  }
}

class _StructurePainter extends CustomPainter {
  _StructurePainter(this.class_);
  final String class_;

  @override
  void paint(Canvas canvas, Size size) {
    final isBridge = class_ == 'BRIDGE';
    final stroke = isBridge ? const Color(0xFF854D0E) : const Color(0xFFB45309);
    final fill = isBridge ? const Color(0xFFFACC15) : const Color(0xFFFDE68A);

    // Badge.
    canvas.drawCircle(const Offset(12, 12), 11,
        Paint()..color = Colors.white.withValues(alpha: 0.85));
    canvas.drawCircle(
        const Offset(12, 12),
        11,
        Paint()
          ..color = stroke
          ..style = PaintingStyle.stroke
          ..strokeWidth = 1);

    Paint line(Color c, double w) => Paint()
      ..color = c
      ..style = PaintingStyle.stroke
      ..strokeWidth = w
      ..strokeCap = StrokeCap.round;

    if (isBridge) {
      // Deck + two arches.
      canvas.drawLine(const Offset(2, 18), const Offset(22, 18), line(stroke, 2.5));
      final arch1 = Path()
        ..moveTo(4, 18)
        ..quadraticBezierTo(4, 11, 10, 11)
        ..quadraticBezierTo(16, 11, 16, 18);
      final arch2 = Path()
        ..moveTo(12, 18)
        ..quadraticBezierTo(12, 13, 16, 13)
        ..quadraticBezierTo(20, 13, 20, 18);
      for (final arch in [arch1, arch2]) {
        canvas.drawPath(arch, Paint()..color = fill);
        canvas.drawPath(arch, line(stroke, 1.5));
      }
    } else {
      // Overhead utility: sky line, two drops, and an ↕ clearance hint.
      final accent =
          class_ == 'PIPOHD' ? const Color(0xFF7C2D12) : stroke;
      canvas.drawLine(const Offset(2, 7), const Offset(22, 7), line(accent, 2));
      canvas.drawLine(const Offset(6, 7), const Offset(6, 19), line(accent, 1.5));
      canvas.drawLine(const Offset(18, 7), const Offset(18, 19), line(accent, 1.5));
      final hint = Path()
        ..moveTo(12, 9)
        ..lineTo(12, 19)
        ..moveTo(9, 12)
        ..lineTo(12, 9)
        ..lineTo(15, 12)
        ..moveTo(9, 16)
        ..lineTo(12, 19)
        ..lineTo(15, 16);
      canvas.drawPath(hint, line(accent, 1.2)..strokeCap = StrokeCap.butt);
    }
  }

  @override
  bool shouldRepaint(covariant _StructurePainter old) =>
      old.class_ != class_;
}
