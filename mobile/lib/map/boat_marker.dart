import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_svg/flutter_svg.dart';

import 'boat_icon.dart';

/// The own-boat marker (C1): the web app's top-down boat SVG, rotated to
/// heading, sized by [MyBoatIcon.renderedSize] — or the operator's
/// /myboat-icon override when the module exposes one. Only the own boat
/// uses the override; AIS targets keep their own icon (D5).
class BoatMarker extends StatelessWidget {
  const BoatMarker({
    super.key,
    required this.headingDeg,
    this.sx = 1,
    this.sy = 1,
  });

  final double headingDeg;
  final double sx; // across the boat (beam)
  final double sy; // along the boat (length)

  @override
  Widget build(BuildContext context) {
    final size = MyBoatIcon.renderedSize(sx, sy);
    final bytes = MyBoatIcon.overrideBytes;
    final icon = bytes != null
        ? Image.memory(bytes,
            width: size.width, height: size.height, fit: BoxFit.fill)
        : SvgPicture.asset('assets/topdown-boat.svg',
            width: size.width, height: size.height, fit: BoxFit.fill);
    return Transform.rotate(
      angle: headingDeg * math.pi / 180.0,
      child: icon,
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
