import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/cpa.dart';

void main() {
  test('head-on closing: CPA ~0, TCPA = distance/closing speed', () {
    // Own northbound at 10 kn, target 2 nm due north coming south at 10 kn.
    // Closing at 20 kn over 2 nm → 6 minutes, meeting head-on (CPA ≈ 0).
    final r = computeCpa(
      ownLat: 41.0,
      ownLng: -72.0,
      ownCogDeg: 0,
      ownSpdKn: 10,
      tgtLat: 41.0 + 2.0 / 60.0,
      tgtLng: -72.0,
      tgtCogDeg: 180,
      tgtSpdKn: 10,
    )!;
    expect(r.cpaNm, closeTo(0, 0.02));
    expect(r.tcpaMin, closeTo(6, 0.05));
  });

  test('parallel same course and speed: no relative motion → null', () {
    final r = computeCpa(
      ownLat: 41.0,
      ownLng: -72.0,
      ownCogDeg: 90,
      ownSpdKn: 8,
      tgtLat: 41.01,
      tgtLng: -72.0,
      tgtCogDeg: 90,
      tgtSpdKn: 8,
    );
    expect(r, isNull);
  });

  test('crossing target passes ahead with a nonzero CPA', () {
    // Target 1 nm east heading north at 10 kn; own northbound at 5 kn.
    final r = computeCpa(
      ownLat: 41.0,
      ownLng: -72.0,
      ownCogDeg: 0,
      ownSpdKn: 5,
      tgtLat: 41.0,
      tgtLng: -72.0 + 1.0 / 60.0 / 0.7547, // ≈1 nm east at 41°N
      tgtCogDeg: 0,
      tgtSpdKn: 10,
    );
    // Same course, different speed → relative motion is pure north; the
    // eastward offset never closes: CPA = initial lateral distance.
    expect(r, isNotNull);
    expect(r!.cpaNm, closeTo(1.0, 0.02));
  });

  test('already-diverging target reports a negative TCPA', () {
    // Target 2 nm north, heading further north faster than own.
    final r = computeCpa(
      ownLat: 41.0,
      ownLng: -72.0,
      ownCogDeg: 0,
      ownSpdKn: 5,
      tgtLat: 41.0 + 2.0 / 60.0,
      tgtLng: -72.0,
      tgtCogDeg: 0,
      tgtSpdKn: 15,
    )!;
    expect(r.tcpaMin, lessThan(0)); // UI hides it — past its CPA
  });

  test('missing COG on either vessel → null, never a wrong number', () {
    expect(
        computeCpa(
            ownLat: 41,
            ownLng: -72,
            ownCogDeg: null,
            ownSpdKn: 5,
            tgtLat: 41.1,
            tgtLng: -72,
            tgtCogDeg: 90,
            tgtSpdKn: 5),
        isNull);
    expect(
        computeCpa(
            ownLat: 41,
            ownLng: -72,
            ownCogDeg: 0,
            ownSpdKn: 5,
            tgtLat: 41.1,
            tgtLng: -72,
            tgtCogDeg: null,
            tgtSpdKn: 5),
        isNull);
  });
}
