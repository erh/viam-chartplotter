import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/map/boat_icon.dart';

void main() {
  tearDown(() {
    MyBoatIcon.overrideBytes = null;
    MyBoatIcon.naturalWidth = null;
    MyBoatIcon.naturalHeight = null;
  });

  test('dimScaleFactor: sqrt-of-ratio, clamped, 1 on unknown', () {
    expect(dimScaleFactor(null, defaultBoatLengthM), 1);
    expect(dimScaleFactor(0, defaultBoatLengthM), 1);
    expect(dimScaleFactor(defaultBoatLengthM, defaultBoatLengthM),
        closeTo(1, 1e-9));
    // 4x the reference → sqrt(4) = 2.
    expect(dimScaleFactor(defaultBoatLengthM * 4, defaultBoatLengthM),
        closeTo(2, 1e-9));
    // Clamps: a dinghy and a supertanker.
    expect(dimScaleFactor(1, defaultBoatLengthM), boatScaleMin);
    expect(dimScaleFactor(2000, defaultBoatLengthM), boatScaleMax);
  });

  test('an 800 ft vessel with no beam grows long, not wide', () {
    final axes = boatScaleAxes(243.8, null); // 800 ft
    expect(axes.sy, boatScaleMax); // clamped long axis
    expect(axes.sx, 1); // beam unknown → cross-axis untouched
  });

  test('bundled icon renders at natural size times the axes', () {
    final s = MyBoatIcon.renderedSize(1, 2);
    expect(s.width, boatImageNaturalWidth);
    expect(s.height, boatImageNaturalHeight * 2);
  });

  test('override is height-matched to the bundled 73px', () {
    MyBoatIcon.overrideBytes = Uint8List(1);
    MyBoatIcon.naturalWidth = 292; // 4:1 the bundled height
    MyBoatIcon.naturalHeight = 292;
    final s = MyBoatIcon.renderedSize(1, 1);
    expect(s.height, closeTo(boatImageNaturalHeight, 1e-6));
    expect(s.width, closeTo(73, 1e-6)); // square source stays square
  });

  test('narrow override is bumped to the minimum rendered width', () {
    MyBoatIcon.overrideBytes = Uint8List(1);
    MyBoatIcon.naturalWidth = 20; // sliver: 20/300 aspect
    MyBoatIcon.naturalHeight = 300;
    final s = MyBoatIcon.renderedSize(1, 1);
    expect(s.width, greaterThanOrEqualTo(myBoatMinRenderedWidthPx - 1e-6));
    // Aspect preserved: height grew by the same bump.
    expect(s.height / s.width, closeTo(300 / 20, 1e-6));
  });
}
