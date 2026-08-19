import 'dart:math' as math;

/// Approximate NOAA ENC coverage (US marine waters), ported verbatim from
/// the web app (src/marineMap.svelte:101-131). Used to skip the under-chart
/// OSM fallback fetch where the chart already covers the tile. Generous
/// rectangles as `[lonW, latS, lonE, latN]`; tiles only partly inside
/// (coverage edges) still load OSM, so no blank gaps at the boundary.
const List<List<double>> usEncCoverage = [
  [-128, 22, -64, 50], // CONUS + Atlantic/Gulf/Pacific coasts + Great Lakes
  [-180, 50, -128, 73], // Alaska (mainland + eastern Aleutians)
  [172, 50, 180, 73], //   Alaska (Aleutians across the dateline)
  [-161, 18, -154, 23], // Hawaii
  [-68.2, 17.3, -64.2, 19.1], // Puerto Rico / USVI
  [144, 13, 146.5, 21], // Guam / CNMI
  [-171.5, -14.6, -168, -10.8], // American Samoa
];

/// True when the whole XYZ tile falls inside US ENC coverage (web-mercator
/// tile → lon/lat box, fully-contained test).
bool tileFullyInUSWaters(int z, int x, int y) {
  final n = math.pow(2, z).toDouble();
  final lonW = (x / n) * 360 - 180;
  final lonE = ((x + 1) / n) * 360 - 180;
  double latOfY(int ty) =>
      math.atan(_sinh(math.pi * (1 - (2 * ty) / n))) * 180 / math.pi;
  final latN = latOfY(y);
  final latS = latOfY(y + 1);
  return usEncCoverage.any(
      (b) => lonW >= b[0] && lonE <= b[2] && latS >= b[1] && latN <= b[3]);
}

double _sinh(double v) => (math.exp(v) - math.exp(-v)) / 2;

/// XYZ tile containing a lon/lat at zoom [z] — test/debug helper.
({int x, int y}) tileAt(double lon, double lat, int z) {
  final n = math.pow(2, z).toDouble();
  final x = ((lon + 180) / 360 * n).floor();
  final latRad = lat * math.pi / 180;
  final y =
      ((1 - math.log(math.tan(latRad) + 1 / math.cos(latRad)) / math.pi) /
              2 *
              n)
          .floor();
  return (x: x, y: y);
}
