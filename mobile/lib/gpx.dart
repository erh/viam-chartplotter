import 'package:latlong2/latlong.dart';

/// GPX 1.1 export of a waypoint list (E4), ported verbatim from
/// src/lib/gpx.ts (tests translated alongside). Shaped for chartplotter
/// import: Garmin units read routes from Garmin/GPX/*.gpx on a memory card.
/// The route is emitted as one <rte> whose <rtept>s carry sequential names
/// so they're easy to identify after import.

String _escapeXml(String s) => s
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&apos;');

/// Waypoint names inside the route: "<prefix> 01", "<prefix> 02", … where
/// the prefix is the route name truncated to keep names short on plotter
/// screens.
String _pointName(String routeName, int i) {
  var prefix = routeName.trim();
  if (prefix.length > 20) prefix = prefix.substring(0, 20);
  if (prefix.isEmpty) prefix = 'WP';
  return '$prefix ${(i + 1).toString().padLeft(2, '0')}';
}

String routeToGpx(String name, List<LatLng> waypoints) {
  final displayName = name.trim().isEmpty ? 'Route' : name.trim();
  final pts = [
    for (var i = 0; i < waypoints.length; i++)
      '    <rtept lat="${waypoints[i].latitude.toStringAsFixed(7)}" '
          'lon="${waypoints[i].longitude.toStringAsFixed(7)}">\n'
          '      <name>${_escapeXml(_pointName(displayName, i))}</name>\n'
          '    </rtept>'
  ].join('\n');
  return '<?xml version="1.0" encoding="UTF-8"?>\n'
      '<gpx version="1.1" creator="viam-chartplotter" '
      'xmlns="http://www.topografix.com/GPX/1/1">\n'
      '  <rte>\n'
      '    <name>${_escapeXml(displayName)}</name>\n'
      '$pts\n'
      '  </rte>\n'
      '</gpx>\n';
}

/// Filesystem-safe file name derived from the route name.
String gpxFilename(String name) {
  var slug = name
      .trim()
      .toLowerCase()
      .replaceAll(RegExp(r'[^a-z0-9]+'), '-')
      .replaceAll(RegExp(r'^-+|-+$'), '');
  if (slug.isEmpty) slug = 'route';
  return '$slug.gpx';
}
