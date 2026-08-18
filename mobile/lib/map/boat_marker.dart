import 'dart:math' as math;

import 'package:flutter/material.dart';

/// The own-boat marker, rotated to heading.
///
/// Task C1 replaces the generic icon with the web app's top-down boat SVG,
/// scaled by the vessel's length and beam, plus the operator's /myboat-icon
/// override — all of which lands here rather than in the map screen.
class BoatMarker extends StatelessWidget {
  const BoatMarker({super.key, required this.headingDeg});
  final double headingDeg;

  @override
  Widget build(BuildContext context) {
    return Transform.rotate(
      angle: headingDeg * math.pi / 180.0,
      child: const Icon(Icons.navigation, color: Colors.red, size: 36),
    );
  }
}
