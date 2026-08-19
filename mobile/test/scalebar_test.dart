import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/map/nautical_scalebar.dart';

void main() {
  test('picks the longest nice length that fits', () {
    // 10 m/px: 1 nm = 185.2 px (too wide), 0.5 nm = 92.6 px → fits.
    final bar = scalebarFor(10)!;
    expect(bar.nm, 0.5);
    expect(bar.px, closeTo(92.6, 0.1));
  });

  test('updates with zoom (coarser scale → longer nm)', () {
    final zoomedIn = scalebarFor(2)!; // fine
    final zoomedOut = scalebarFor(200)!; // coarse
    expect(zoomedOut.nm, greaterThan(zoomedIn.nm));
  });

  test('degenerate scales return null instead of dividing by zero', () {
    expect(scalebarFor(0), isNull);
    expect(scalebarFor(double.nan), isNull);
    expect(scalebarFor(1e9), isNull); // nothing fits
  });

  test('labels read in nm', () {
    expect(scalebarLabel(5), '5 nm');
    expect(scalebarLabel(0.5), '0.5 nm');
  });
}
