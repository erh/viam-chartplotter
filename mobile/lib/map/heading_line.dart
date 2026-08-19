import 'package:latlong2/latlong.dart';

/// Heading-line geometry (C4): from the boat along its *heading* (not COG)
/// for the selected distance. Proper great-circle offsets, sampled into
/// segments — at 15 nm a flat-earth delta is visibly wrong.
List<LatLng> headingLinePoints(
  LatLng from,
  double headingDeg,
  double lengthNm, {
  int segments = 8,
}) {
  const d = Distance();
  final meters = lengthNm * 1852.0;
  return [
    for (var i = 0; i <= segments; i++)
      d.offset(from, meters * i / segments, headingDeg),
  ];
}

/// Selectable lengths, matching the web app's options (default 5 nm).
const List<int> headingLineLengthChoices = [1, 2, 3, 5, 10, 15];
