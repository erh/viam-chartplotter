import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../boat_state.dart';
import '../chart/areas.dart';
import '../chart/structures.dart';
import '../tile_sources.dart';
import '../weather.dart';
import 'boat_marker.dart';
import 'osm_underlay.dart';
import 'weather_particles.dart';

/// Builds the FlutterMap child stack, bottom to top: chart tiles, weather,
/// route, AIS, own boat.
///
/// This is the busiest seam in the port — most chart tasks (A1 tile params,
/// A5 the OSM underlay, B1/B2 navaid + structure vectors, C2 the track,
/// D2 projection vectors, D10 the AIS cull) add or change a layer here. Keeping
/// it out of MapScreen means those tasks don't all edit the same file.
List<Widget> buildMapLayers({
  required BoatState state,
  required TileSource base,
  // Weather particle fields (null = overlay off / zoom-gated). Rendered as
  // ol-wind-style animated particles, matching the web app's display.
  WindField? windField,
  WindField? waveField,
  // Isobar contours + labels/extrema (F4), between waves and wind.
  List<Polyline> isobarLines = const [],
  List<Marker> isobarMarkers = const [],
  required List<Marker> aisMarkers,
  List<AreaInfo> areas = const [],
  List<Marker> navaidMarkers = const [],
  List<Marker> structureMarkers = const [],
  List<StructureFeature> structures = const [],
  List<List<LatLng>> aisProjections = const [],
  List<List<LatLng>> aisTracks = const [],
  List<LatLng> ownProjection = const [],
  // Saved-route previews (E2): display-only, dashed so they can never be
  // mistaken for the active (solid) route.
  List<({List<LatLng> points, Color color})> routePreviews = const [],
  // Active nav-service route (E1): the waypoint chain (boat prepended by the
  // caller when it has a fresh fix) and the tappable waypoint markers.
  List<LatLng> activeRoutePoints = const [],
  List<Marker> waypointMarkers = const [],
  required List<({List<LatLng> points, Color color})> trackSegments,
  List<LatLng> headingLine = const [],
  List<List<LatLng>> headingTicks = const [],
  required String buildStamp,
  required double Function() viewZoom,
  int? safeDepthFt,
}) {
  return [
    // Under-chart OSM fallback (A5): charts only cover US waters, so keep
    // OSM underneath and let the provider suppress the fetch for tiles the
    // opaque chart fully covers.
    if (base.isChart)
      TileLayer(
        key: const ValueKey('osm-underlay'),
        urlTemplate: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png',
        tileProvider: OsmUnderlayTileProvider(viewZoom: viewZoom),
        maxNativeZoom: 19,
        userAgentPackageName: 'com.viam.chartplotter.mobile',
        errorTileCallback: (tile, error, stackTrace) =>
            debugPrint('osm underlay tile failed: $error'),
      ),
    TileLayer(
      // The safe depth participates in the key so changing it drops
      // flutter_map's in-memory tile cache and refetches the reshaded
      // chart (A2).
      key: ValueKey('${base.id}-sd${safeDepthFt ?? ''}'),
      urlTemplate: base.urlTemplate,
      // Chart tiles vary their query params by tile zoom (A1) and carry the
      // cache-buster + safe depth (A3/A2); other layers use the plain
      // template. Both go through the disk cache (L1).
      tileProvider: base.paramsForZoom == null
          ? CachingTileProvider()
          : ZoomParamsTileProvider((z) => chartTileUrlParams(z,
              buildStamp: buildStamp, safeDepthFt: safeDepthFt)),
      minZoom: base.minZoom.toDouble(),
      maxNativeZoom: base.maxZoom,
      userAgentPackageName: 'com.viam.chartplotter.mobile',
      // flutter_map shows nothing for a failed tile, so a broken URL/host
      // reads as a blank map — log it instead.
      errorTileCallback: (tile, error, stackTrace) =>
          debugPrint('tile load failed (${base.id}): $error'),
    ),
    // Regions from area components (B3): translucent fill + solid outline
    // in the area's color, drawn under the boat/AIS vectors and above the
    // chart tiles (matching the web's layer order). Points render as dots.
    if (areas.isNotEmpty) ...[
      PolygonLayer(
        polygons: [
          for (final a in areas)
            for (final g in a.geoms)
              if (!g.isPoint && !g.isLine)
                Polygon(
                  points: g.points,
                  color: g.color.withValues(alpha: areaFillAlpha),
                  borderColor: g.color,
                  borderStrokeWidth: 2,
                ),
        ],
      ),
      PolylineLayer(
        polylines: [
          for (final a in areas)
            for (final g in a.geoms)
              if (g.isLine)
                Polyline(points: g.points, color: g.color, strokeWidth: 2),
        ],
      ),
      MarkerLayer(
        markers: [
          for (final a in areas)
            for (final g in a.geoms)
              if (g.isPoint)
                Marker(
                  point: g.points.first,
                  width: 12,
                  height: 12,
                  child: DecoratedBox(
                    decoration: BoxDecoration(
                      shape: BoxShape.circle,
                      color: g.color.withValues(alpha: areaFillAlpha),
                      border: Border.all(color: g.color, width: 2),
                    ),
                  ),
                ),
        ],
      ),
    ],
    // Own-boat live track (C2), under everything that moves. One polyline
    // per same-colour run; depth mode colours shoal transits red.
    if (trackSegments.isNotEmpty)
      PolylineLayer(
        polylines: [
          for (final s in trackSegments)
            Polyline(points: s.points, color: s.color, strokeWidth: 1.5),
        ],
      ),
    // Heading line (C4): from the boat along its heading, with a
    // perpendicular tick every 1 nm so distance reads at a glance.
    if (headingLine.length >= 2)
      PolylineLayer(
        polylines: [
          Polyline(
            points: headingLine,
            color: Colors.redAccent,
            strokeWidth: 2,
          ),
          for (final tick in headingTicks)
            Polyline(points: tick, color: Colors.redAccent, strokeWidth: 2),
        ],
      ),
    // Saved-route previews (E2): dashed, in each route's colour, with a dot
    // on the first waypoint so direction reads at a glance.
    if (routePreviews.isNotEmpty) ...[
      PolylineLayer(
        polylines: [
          for (final p in routePreviews)
            if (p.points.length >= 2)
              Polyline(
                points: p.points,
                color: p.color,
                strokeWidth: 3,
                pattern: StrokePattern.dashed(segments: const [10, 6]),
              ),
        ],
      ),
      MarkerLayer(
        markers: [
          for (final p in routePreviews)
            if (p.points.isNotEmpty)
              Marker(
                point: p.points.first,
                width: 14,
                height: 14,
                child: DecoratedBox(
                  decoration: BoxDecoration(
                    shape: BoxShape.circle,
                    color: p.color,
                    border: Border.all(color: Colors.black54, width: 2),
                  ),
                ),
              ),
        ],
      ),
    ],
    // Wave field (F3): translucent height-coloured cells under the wind
    // arrows so both weather overlays read together.
    // Wave particles (F3): the web's ol-wind rendering — drift shows
    // propagation, colour shows height.
    if (waveField != null)
      WeatherParticleLayer(
          field: waveField, config: WeatherParticleConfig.waves),
    // Isobars (F4): PRMSL contours with sparse pressure labels and H/L
    // extremum marks, over the wave field, under the wind particles.
    if (isobarLines.isNotEmpty) PolylineLayer(polylines: isobarLines),
    if (isobarMarkers.isNotEmpty) MarkerLayer(markers: isobarMarkers),
    // Wind particles, over the chart, under boat markers.
    if (windField != null)
      WeatherParticleLayer(
          field: windField, config: WeatherParticleConfig.wind),
    // Active route. With nav-service waypoints (E1) the full chain draws
    // solid purple; otherwise fall back to the route sensor's boat→destination
    // line (an MFD-driven route the nav service doesn't know about).
    if (activeRoutePoints.length >= 2)
      PolylineLayer(
        polylines: [
          Polyline(
            points: activeRoutePoints,
            strokeWidth: 3,
            color: Colors.purpleAccent,
          ),
        ],
      )
    else if (state.position != null && state.destination != null)
      PolylineLayer(
        polylines: [
          Polyline(
            points: [state.position!, state.destination!],
            strokeWidth: 3,
            color: Colors.purpleAccent,
          ),
        ],
      ),
    if (waypointMarkers.isEmpty && state.destination != null)
      MarkerLayer(
        markers: [
          Marker(
            point: state.destination!,
            width: 30,
            height: 30,
            child:
                const Icon(Icons.flag, color: Colors.purpleAccent, size: 26),
          ),
        ],
      ),
    // Structure traces (B2): at z>=14 the tile skips these classes, so
    // this vector is the SOLE renderer — bridges draw as a solid amber
    // bar with a dark outline, overhead cables/pipes/conveyors as a thick
    // dashed line (the dash reads as "overhead").
    if (structures.any((f) => f.traces.isNotEmpty)) ...[
      PolygonLayer(
        polygons: [
          for (final f in structures)
            if (f.isPolygon)
              for (final ring in f.traces)
                Polygon(
                  points: ring,
                  color: f.isBridge
                      ? const Color(0xFFFACC15).withValues(alpha: 0.55)
                      : const Color(0xFFFDE68A).withValues(alpha: 0.4),
                  borderColor: f.isBridge
                      ? const Color(0xF2783510)
                      : const Color(0xF29A3412),
                  borderStrokeWidth: 3,
                ),
        ],
      ),
      PolylineLayer(
        polylines: [
          for (final f in structures)
            if (!f.isPolygon)
              for (final line in f.traces)
                Polyline(
                  points: line,
                  color: f.isBridge
                      ? const Color(0xF2783510)
                      : const Color(0xF29A3412),
                  strokeWidth: 3,
                  pattern: f.isBridge
                      ? const StrokePattern.solid()
                      : StrokePattern.dashed(segments: const [8, 5]),
                ),
        ],
      ),
    ],
    // Navaids (B1): tappable buoys/beacons/lights, above the chart tiles
    // and under the traffic picture.
    if (navaidMarkers.isNotEmpty) MarkerLayer(markers: navaidMarkers),
    // Structure badges (B2), tappable for clearances.
    if (structureMarkers.isNotEmpty) MarkerLayer(markers: structureMarkers),
    // AIS history tracks (D1), dim, under the projections and markers.
    if (aisTracks.isNotEmpty)
      PolylineLayer(
        polylines: [
          for (final t in aisTracks)
            Polyline(
              points: t,
              color: Colors.cyan.withValues(alpha: 0.35),
              strokeWidth: 1.5,
            ),
        ],
      ),
    // Projection vectors (D2): thin lines ahead of each target — and own
    // boat, in green — showing position in N minutes along COG at SOG.
    if (aisProjections.isNotEmpty || ownProjection.length >= 2)
      PolylineLayer(
        polylines: [
          for (final p in aisProjections)
            Polyline(
              points: p,
              color: Colors.cyanAccent.withValues(alpha: 0.7),
              strokeWidth: 1.5,
            ),
          if (ownProjection.length >= 2)
            Polyline(
              points: ownProjection,
              color: Colors.greenAccent,
              strokeWidth: 2,
            ),
        ],
      ),
    // AIS targets (drawn under the own-boat marker). Prebuilt and cached by
    // MapScreen (D10): culled to the viewport, capped, rebuilt only when the
    // AIS set or the camera changes — not on every 1 Hz state tick.
    if (aisMarkers.isNotEmpty) MarkerLayer(markers: aisMarkers),
    // Waypoint markers (E1): above the traffic picture — these are what the
    // helmsman is steering to and editing.
    if (waypointMarkers.isNotEmpty) MarkerLayer(markers: waypointMarkers),
    if (state.displayPosition != null)
      MarkerLayer(
        markers: [
          // Phone-GPS fallback (L5) renders as an unmistakably different
          // marker — a helmsman must never confuse the phone's fix with the
          // boat's.
          Marker(
            point: state.displayPosition!,
            width: state.usingPhoneGps ? 26 : 40,
            height: state.usingPhoneGps ? 26 : 40,
            // Markers default to rotate:false in flutter_map, which means
            // they render INSIDE the rotated map space and already turn
            // with the chart — the child angle must be the plain map-north
            // heading. Adding the camera rotation here double-counted it,
            // so in course-up the boat matched neither COG nor heading.
            child: state.usingPhoneGps
                ? const PhoneGpsMarker()
                : BoatMarker(headingDeg: state.effectiveHeadingDeg ?? 0),
          ),
        ],
      ),
  ];
}
