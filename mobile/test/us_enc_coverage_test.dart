import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/map/us_enc_coverage.dart';

void main() {
  test('mid-Atlantic tile is outside US coverage', () {
    final t = tileAt(-40.0, 35.0, 9);
    expect(tileFullyInUSWaters(9, t.x, t.y), isFalse);
  });

  test('Long Island Sound tile is inside', () {
    final t = tileAt(-72.6, 41.1, 9);
    expect(tileFullyInUSWaters(9, t.x, t.y), isTrue);
  });

  test('a tile straddling the coverage edge still loads OSM', () {
    // The CONUS box's west edge is -128: a low-zoom tile containing that
    // meridian is only partly inside → not "fully in US waters".
    final t = tileAt(-128.0, 40.0, 5);
    expect(tileFullyInUSWaters(5, t.x, t.y), isFalse);
  });

  test('Aleutian tiles on either side of the dateline are inside', () {
    final west = tileAt(175.0, 52.0, 8); // west of the dateline (172..180 box)
    expect(tileFullyInUSWaters(8, west.x, west.y), isTrue);
    final east = tileAt(-175.0, 52.0, 8); // east side (-180..-128 box)
    expect(tileFullyInUSWaters(8, east.x, east.y), isTrue);
  });

  test('the Med is outside (foreign waters keep their OSM land)', () {
    final t = tileAt(9.19, 41.9, 9); // Tyrrhenian Sea
    expect(tileFullyInUSWaters(9, t.x, t.y), isFalse);
  });
}
