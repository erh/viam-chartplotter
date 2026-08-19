import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:latlong2/latlong.dart';

/// Regions from `area` components (B3) — e.g. the rw-* right-whale seasonal
/// management areas. Ported from the web app (src/App.svelte:1068-1147,
/// src/marineMap.svelte:893-951, 1761-1789): each area component answers the
/// {"get_area": true} DoCommand with a normalized GeoJSON FeatureCollection
/// plus color and an optional recurring month-day season window.

/// Matches the Go module's defaultAreaColor (area.go).
const Color areaDefaultColor = Color(0xFFFF3B30);

/// Web: AREA_FILL_ALPHA — the translucent interior; outline is full opacity.
const double areaFillAlpha = 0.2;

/// Today's local month-day as MM-DD.
String localTodayMMDD([DateTime? now]) {
  final d = now ?? DateTime.now();
  String p(int n) => n.toString().padLeft(2, '0');
  return '${p(d.month)}-${p(d.day)}';
}

/// Whether an area with the given inclusive month-day window should show
/// today. MM-DD strings compare lexicographically in calendar order; when
/// both are set and start > end the window wraps the year end (e.g.
/// 11-01..04-30 for a winter closure).
bool areaVisibleToday(String? startDate, String? endDate, {String? today}) {
  final t = today ?? localTodayMMDD();
  final start = (startDate != null && startDate.isNotEmpty) ? startDate : null;
  final end = (endDate != null && endDate.isNotEmpty) ? endDate : null;
  if (start != null && end != null) {
    return start.compareTo(end) <= 0
        ? t.compareTo(start) >= 0 && t.compareTo(end) <= 0
        : t.compareTo(start) >= 0 || t.compareTo(end) <= 0;
  }
  if (start != null && t.compareTo(start) < 0) return false;
  if (end != null && t.compareTo(end) > 0) return false;
  return true;
}

/// The month-day window suffix for a toggle row, so an out-of-season area
/// shows why it starts unchecked (web areaDateSuffix).
String areaDateSuffix(String? startDate, String? endDate) {
  final s = (startDate != null && startDate.isNotEmpty) ? startDate : null;
  final e = (endDate != null && endDate.isNotEmpty) ? endDate : null;
  if (s != null && e != null) return ' ($s → $e)';
  if (s != null) return ' (from $s)';
  if (e != null) return ' (until $e)';
  return '';
}

/// #rgb / #rrggbb CSS color, else [fallback].
Color parseCssColor(String? s, {Color fallback = areaDefaultColor}) {
  if (s == null) return fallback;
  final hex = s.trim();
  if (hex.startsWith('#')) {
    final body = hex.substring(1);
    String full;
    if (body.length == 3) {
      full = body.split('').map((c) => '$c$c').join();
    } else if (body.length == 6) {
      full = body;
    } else {
      return fallback;
    }
    final v = int.tryParse(full, radix: 16);
    if (v != null) return Color(0xFF000000 | v);
  }
  return fallback;
}

/// One drawable piece of an area.
class AreaGeometry {
  const AreaGeometry.polygon(this.points, this.color) : isPoint = false, isLine = false;
  const AreaGeometry.line(this.points, this.color)
      : isPoint = false,
        isLine = true;
  const AreaGeometry.point(this.points, this.color)
      : isPoint = true,
        isLine = false;
  final List<LatLng> points; // ring for polygons, path for lines, [p] for points
  final Color color;
  final bool isPoint;
  final bool isLine;
}

/// One `area` component's payload.
class AreaInfo {
  const AreaInfo({
    required this.name,
    required this.color,
    required this.geoms,
    this.startDate,
    this.endDate,
    required this.inSeason,
    this.folder,
  });
  final String name;
  final Color color;
  final List<AreaGeometry> geoms;
  final String? startDate;
  final String? endDate;
  final bool inSeason;
  final String? folder;
}

List<LatLng>? _coordsToRing(dynamic coords) {
  if (coords is! List) return null;
  final pts = <LatLng>[];
  for (final c in coords) {
    if (c is! List || c.length < 2) continue;
    final lon = (c[0] as num?)?.toDouble();
    final lat = (c[1] as num?)?.toDouble();
    if (lon == null || lat == null) continue;
    pts.add(LatLng(lat, lon));
  }
  return pts.isEmpty ? null : pts;
}

/// Parse an area's GeoJSON (Geometry, Feature, or FeatureCollection — the
/// module normalizes to a FeatureCollection, but accept all three like the
/// web). Per-feature `color` properties override the area default.
List<AreaGeometry> parseAreaGeoJson(dynamic geojson, Color defaultColor) {
  final out = <AreaGeometry>[];
  void addGeometry(dynamic geom, Color color) {
    if (geom is! Map) return;
    final type = geom['type'];
    final coords = geom['coordinates'];
    switch (type) {
      case 'Point':
        final p = _coordsToRing([coords]);
        if (p != null) out.add(AreaGeometry.point(p, color));
      case 'LineString':
        final l = _coordsToRing(coords);
        if (l != null && l.length >= 2) out.add(AreaGeometry.line(l, color));
      case 'MultiLineString':
        if (coords is List) {
          for (final l in coords) {
            final r = _coordsToRing(l);
            if (r != null && r.length >= 2) out.add(AreaGeometry.line(r, color));
          }
        }
      case 'Polygon':
        if (coords is List && coords.isNotEmpty) {
          final outer = _coordsToRing(coords.first);
          if (outer != null && outer.length >= 3) {
            out.add(AreaGeometry.polygon(outer, color));
          }
        }
      case 'MultiPolygon':
        if (coords is List) {
          for (final poly in coords) {
            if (poly is List && poly.isNotEmpty) {
              final outer = _coordsToRing(poly.first);
              if (outer != null && outer.length >= 3) {
                out.add(AreaGeometry.polygon(outer, color));
              }
            }
          }
        }
      case 'GeometryCollection':
        final geoms = geom['geometries'];
        if (geoms is List) {
          for (final g in geoms) {
            addGeometry(g, color);
          }
        }
    }
  }

  void addFeature(dynamic f) {
    if (f is! Map) return;
    final props = f['properties'];
    final color = (props is Map && props['color'] != null)
        ? parseCssColor(props['color'].toString(), fallback: defaultColor)
        : defaultColor;
    addGeometry(f['geometry'], color);
  }

  dynamic root = geojson;
  if (root is String) {
    try {
      root = jsonDecode(root);
    } catch (_) {
      return const [];
    }
  }
  if (root is! Map) return const [];
  switch (root['type']) {
    case 'FeatureCollection':
      final features = root['features'];
      if (features is List) features.forEach(addFeature);
    case 'Feature':
      addFeature(root);
    default:
      addGeometry(root, defaultColor); // bare Geometry
  }
  return out;
}

/// Component name → config-folder name, from the authored config's
/// `ui_folder` markers (a cloud concept viam-server strips before modules
/// see it — the authored config JSON is the only place to learn it). The
/// web also merges fragment configs; mobile reads the part's own list.
Map<String, String> componentFolders(String robotConfigJson) {
  if (robotConfigJson.isEmpty) return const {};
  try {
    final cfg = jsonDecode(robotConfigJson);
    final comps = (cfg as Map)['components'];
    if (comps is! List) return const {};
    final out = <String, String>{};
    for (final c in comps) {
      if (c is! Map) continue;
      final name = c['name'];
      final folder = c['ui_folder'];
      if (name is String &&
          folder is Map &&
          folder['name'] is String &&
          (folder['name'] as String).isNotEmpty) {
        out[name] = folder['name'] as String;
      }
    }
    return out;
  } catch (_) {
    return const {};
  }
}
