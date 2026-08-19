import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;
import 'package:latlong2/latlong.dart';

/// Navaid data + S-57 decoding (B1), ported from the web app
/// (src/marineMap.svelte:1512-1959). The lookup tables are the bulk of the
/// value and the easiest thing to get subtly wrong — they are verbatim.

/// S-57 COLOUR code → chart colour (web s57ColourToCss).
Color s57Colour(String code) {
  switch (code.trim()) {
    case '1':
      return const Color(0xFFFFFFFF); // white
    case '2':
      return const Color(0xFF000000); // black
    case '3':
      return const Color(0xFFD9263A); // red
    case '4':
      return const Color(0xFF1F9E49); // green
    case '5':
      return const Color(0xFF1446CC); // blue
    case '6':
      return const Color(0xFFF5D011); // yellow
    case '7':
      return const Color(0xFF888888); // grey
    case '8':
      return const Color(0xFF8B5A2B); // brown
    case '9':
      return const Color(0xFFFFA500); // amber
    case '10':
      return const Color(0xFF8246C8); // violet
    case '11':
      return const Color(0xFFFF6E00); // orange
    case '12':
      return const Color(0xFFC850C8); // magenta
    case '13':
      return const Color(0xFFFFB4D2); // pink
    default:
      return const Color(0xFF888888);
  }
}

/// S-52 magenta — light flares on NOAA charts.
const Color navaidLightMagenta = Color(0xFFC850C8);
const Color navaidGrey = Color(0xFF888888);

List<Color> navaidColours(Map<String, dynamic> props) {
  final raw = props['COLOUR'];
  if (raw is! String || raw.isEmpty) return const [navaidGrey];
  return raw.split(',').map(s57Colour).toList();
}

/// Human-readable label for an S-57 class code.
String navaidClassLabel(String c) {
  switch (c) {
    case 'BOYLAT':
      return 'Lateral buoy';
    case 'BOYCAR':
      return 'Cardinal buoy';
    case 'BOYISD':
      return 'Isolated-danger buoy';
    case 'BOYSAW':
      return 'Safe-water buoy';
    case 'BOYSPP':
      return 'Special-purpose buoy';
    case 'BOYINB':
      return 'Installation buoy';
    case 'BCNLAT':
      return 'Lateral beacon';
    case 'BCNCAR':
      return 'Cardinal beacon';
    case 'BCNISD':
      return 'Isolated-danger beacon';
    case 'BCNSAW':
      return 'Safe-water beacon';
    case 'BCNSPP':
      return 'Special-purpose beacon';
    case 'LIGHTS':
      return 'Light';
    case 'DAYMAR':
      return 'Daymark';
    default:
      return c;
  }
}

/// S-57 LITCHR enum → short S-52 code (F, Fl, Q, Iso, Oc, …).
String lightCharLabel(int code) {
  switch (code) {
    case 1:
      return 'F';
    case 2:
    case 3:
      return 'Fl';
    case 4:
      return 'Q';
    case 5:
      return 'VQ';
    case 6:
      return 'UQ';
    case 7:
    case 8:
      return 'Iso';
    case 9:
    case 10:
      return 'Oc';
    case 11:
      return 'Mo';
    case 12:
    case 13:
      return 'FFl';
    default:
      return '';
  }
}

/// S-57 COLOUR csv → single-letter code list (W/R/G/Y/…).
String colourLetters(String csv) {
  const map = {
    '1': 'W',
    '2': 'Bk',
    '3': 'R',
    '4': 'G',
    '5': 'Bu',
    '6': 'Y',
    '7': 'Gy',
    '8': 'Br',
    '9': 'Am',
    '10': 'Vi',
    '11': 'Or',
    '12': 'Mg',
    '13': 'Pk',
  };
  return csv
      .split(',')
      .map((c) => map[c.trim()] ?? '')
      .where((s) => s.isNotEmpty)
      .join();
}

/// The detail-sheet content, mirroring the web tooltip's line structure
/// (formatNavaidTooltip) as plain text: [title, class, …detail lines].
List<String> navaidSheetLines(Map<String, dynamic> props) {
  final class_ = (props['class'] ?? '').toString();
  final lines = <String>[
    props['OBJNAM'] != null
        ? props['OBJNAM'].toString()
        : navaidClassLabel(class_),
    navaidClassLabel(class_),
  ];

  // Light characteristic, e.g. "Fl (2) R 5s 65ft 12nm". A buoy joined
  // server-side to a co-located LIGHTS feature carries LIGHT_* attributes;
  // standalone lights carry them directly.
  final isLighted = class_ == 'LIGHTS' || props['lighted'] == true;
  if (isLighted) {
    final parts = <String>[];
    final litchr = props['LITCHR'] ?? props['LIGHT_LITCHR'];
    if (litchr != null) {
      final lc = lightCharLabel(int.tryParse(litchr.toString()) ?? -1);
      if (lc.isNotEmpty) parts.add(lc);
    }
    final siggrp = props['SIGGRP'] ?? props['LIGHT_SIGGRP'];
    if (siggrp != null) parts.add(siggrp.toString());
    final litColour = props['LIGHT_COLOUR'] ?? props['COLOUR'];
    if (litColour is String) {
      final letters = colourLetters(litColour);
      if (letters.isNotEmpty) parts.add(letters);
    }
    final sigper = props['SIGPER'] ?? props['LIGHT_SIGPER'];
    if (sigper != null) parts.add('${sigper}s');
    final height = props['HEIGHT'] ?? props['LIGHT_HEIGHT'];
    if (height != null) {
      final m = double.tryParse(height.toString());
      if (m != null) parts.add('${(m * 3.28084).round()}ft');
    }
    final valnmr = props['VALNMR'] ?? props['LIGHT_VALNMR'];
    if (valnmr != null) parts.add('${valnmr}nm');
    if (parts.isNotEmpty) lines.add(parts.join(' '));
  }

  final sectr1 = props['SECTR1'] ?? props['LIGHT_SECTR1'];
  final sectr2 = props['SECTR2'] ?? props['LIGHT_SECTR2'];
  if (sectr1 != null && sectr2 != null) {
    lines.add('Sector $sectr1°–$sectr2°');
  }
  if (props['INFORM'] != null) lines.add(props['INFORM'].toString());
  return lines;
}

/// One navaid from /noaa-enc/navaids.
class NavaidFeature {
  const NavaidFeature({required this.pos, required this.props});
  final LatLng pos;
  final Map<String, dynamic> props;

  String get class_ => (props['class'] ?? '').toString();
  bool get lighted => props['lighted'] == true;

  /// Buoy/beacon shape enum for the symbol (BOYSHP/BCNSHP).
  int get shape =>
      int.tryParse(
          (class_.startsWith('BCN') ? props['BCNSHP'] : props['BOYSHP'])
                  ?.toString() ??
              '') ??
      0;
}

/// Parse the endpoint's GeoJSON FeatureCollection of Points.
List<NavaidFeature> parseNavaidGeoJson(String body) {
  try {
    final decoded = jsonDecode(body);
    final features = (decoded as Map)['features'];
    if (features is! List) return const [];
    final out = <NavaidFeature>[];
    for (final f in features) {
      if (f is! Map) continue;
      final geom = f['geometry'];
      if (geom is! Map || geom['type'] != 'Point') continue;
      final coords = geom['coordinates'];
      if (coords is! List || coords.length < 2) continue;
      final lon = (coords[0] as num?)?.toDouble();
      final lat = (coords[1] as num?)?.toDouble();
      if (lon == null || lat == null) continue;
      final props = f['properties'];
      out.add(NavaidFeature(
        pos: LatLng(lat, lon),
        props: props is Map
            ? props.map((k, v) => MapEntry(k.toString(), v))
            : const {},
      ));
    }
    return out;
  } catch (_) {
    return const [];
  }
}

/// Fetch for a BboxFeatureSource (B4).
Future<List<NavaidFeature>> fetchNavaids(
  String tileBase,
  double west,
  double south,
  double east,
  double north,
) async {
  final uri = Uri.parse('$tileBase/noaa-enc/navaids'
      '?minLon=$west&minLat=$south&maxLon=$east&maxLat=$north');
  final r = await http.get(uri).timeout(const Duration(seconds: 10));
  if (r.statusCode != 200) return const [];
  return parseNavaidGeoJson(r.body);
}
