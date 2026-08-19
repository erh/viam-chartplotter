import 'dart:math' as math;

import 'package:flutter/material.dart';

/// The own-boat marker, rotated to the effective heading (COG under way).
///
/// C1's scaled top-down boat SVG was tried and reverted — it read worse on
/// the phone than the plain arrow. If it comes back, bring back the sizing
/// helpers and the /myboat-icon override from the C1 commit (df44309).
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

/// The phone's own fix standing in for the boat (L5): an amber dot, nothing
/// like the boat icon, so the source of the fix is never ambiguous.
class PhoneGpsMarker extends StatelessWidget {
  const PhoneGpsMarker({super.key});

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: BoxDecoration(
        shape: BoxShape.circle,
        color: Colors.amber,
        border: Border.all(color: Colors.white, width: 3),
        boxShadow: const [BoxShadow(blurRadius: 4, color: Colors.black54)],
      ),
    );
  }
}
