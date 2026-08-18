import 'package:latlong2/latlong.dart';

/// One AIS target. Mirrors the shape the web app builds in
/// fetchBoatsFromSensor (src/App.svelte) from a viamboat `ais`/`airstream`
/// sensor reading.
class AisBoat {
  const AisBoat({
    required this.mmsi,
    required this.name,
    required this.location,
    required this.sogKn,
    this.headingDeg,
    this.cogDeg,
    this.lengthM,
    this.beamM,
    this.destination,
  });

  final String mmsi;
  final String name;
  final LatLng location;
  final double sogKn;
  final double? headingDeg;
  final double? cogDeg;
  final double? lengthM;
  final double? beamM;
  final String? destination;

  /// Best available orientation for drawing the marker: heading if the vessel
  /// reports one, else course over ground.
  double get orientationDeg => headingDeg ?? cogDeg ?? 0;

  String get displayName => name.isNotEmpty ? name : 'MMSI $mmsi';
}

double? _numOr(dynamic v) => v is num ? v.toDouble() : null;

/// Parse a viamboat AIS sensor reading (map keyed by MMSI) into targets,
/// tolerating the field-name variants across the `ais` and `airstream` models
/// (Cog/COG/Course, Sog/SOG/Speed, Beam/Width). Entries without a valid
/// 2-element Location are skipped.
List<AisBoat> parseAisReadings(Map<String, dynamic> raw) {
  final out = <AisBoat>[];
  raw.forEach((mmsi, value) {
    if (value is! Map) return;
    final loc = value['Location'];
    if (loc is! List || loc.length < 2) return;
    final lat = _numOr(loc[0]);
    final lng = _numOr(loc[1]);
    if (lat == null || lng == null) return;

    final sog = _numOr(value['Sog']) ??
        _numOr(value['SOG']) ??
        _numOr(value['Speed']) ??
        0;
    final cog =
        _numOr(value['Cog']) ?? _numOr(value['COG']) ?? _numOr(value['Course']);
    final heading = _numOr(value['Heading']);
    final length = _numOr(value['Length']);
    final beam = _numOr(value['Beam']) ?? _numOr(value['Width']);
    final dest = value['Destination'];

    out.add(AisBoat(
      mmsi: mmsi,
      name: (value['Name'] is String) ? (value['Name'] as String).trim() : '',
      location: LatLng(lat, lng),
      sogKn: sog,
      headingDeg: (heading != null && heading > 0) ? heading : null,
      cogDeg: cog,
      lengthM: (length != null && length > 0) ? length : null,
      beamM: (beam != null && beam > 0) ? beam : null,
      destination: (dest is String && dest.trim().isNotEmpty)
          ? dest.trim()
          : null,
    ));
  });
  return out;
}
