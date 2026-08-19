import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

/// Scale bar in nautical miles (J7) — flutter_map's built-in Scalebar only
/// speaks metric, and without a scale there's no judging distance at a
/// glance.

/// "Nice" bar lengths, in nm.
const List<double> _niceNm = [
  0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500,
];

/// The longest nice nm length that fits in [maxPx] at [metersPerPixel],
/// with its rendered width. Consecutive nice lengths are within 2.5× of
/// each other, so the [minPx]–[maxPx] window always contains one at sane
/// zooms; degenerate scales (zoomed to a fingernail, or the whole planet a
/// few pixels wide) return null rather than an illegible bar.
({double nm, double px})? scalebarFor(double metersPerPixel,
    {double maxPx = 140, double minPx = 30}) {
  if (metersPerPixel <= 0 || !metersPerPixel.isFinite) return null;
  ({double nm, double px})? best;
  for (final nm in _niceNm) {
    final px = nm * 1852.0 / metersPerPixel;
    if (px <= maxPx && px >= minPx) best = (nm: nm, px: px);
  }
  return best;
}

String scalebarLabel(double nm) =>
    nm >= 1 ? '${nm.toStringAsFixed(0)} nm' : '$nm nm';

/// Map child rendering the bar bottom-left. [liftPx] raises it clear of
/// other bottom overlays (the wind forecast slider).
class NauticalScalebar extends StatelessWidget {
  const NauticalScalebar({super.key, this.liftPx = 0});
  final double liftPx;

  @override
  Widget build(BuildContext context) {
    final camera = MapCamera.of(context);
    // Meters per screen pixel at the view center (rotation-independent).
    final center = camera.center;
    final p = camera.project(center);
    final east = camera.unproject(math.Point(p.x + 100, p.y));
    final metersPerPixel = const Distance().distance(center, east) / 100.0;
    final bar = scalebarFor(metersPerPixel);
    if (bar == null) return const SizedBox.shrink();
    return Align(
      alignment: Alignment.bottomLeft,
      child: Padding(
        padding: EdgeInsets.only(left: 12, bottom: 12 + liftPx),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              scalebarLabel(bar.nm),
              style: const TextStyle(
                color: Colors.white,
                fontSize: 11,
                fontWeight: FontWeight.bold,
                shadows: [Shadow(blurRadius: 2)],
              ),
            ),
            Container(
              width: bar.px,
              height: 6,
              decoration: const BoxDecoration(
                border: Border(
                  left: BorderSide(color: Colors.white, width: 2),
                  bottom: BorderSide(color: Colors.white, width: 2),
                  right: BorderSide(color: Colors.white, width: 2),
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}
