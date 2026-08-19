import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart';

import '../boat_state.dart';
import '../tile_sources.dart';
import 'boat_marker.dart';
import 'osm_underlay.dart';

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
  required double rotationDeg,
  required bool windOn,
  required List<Marker> windMarkers,
  required List<Marker> aisMarkers,
  List<List<LatLng>> aisProjections = const [],
  List<List<LatLng>> aisTracks = const [],
  List<LatLng> ownProjection = const [],
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
      // template with the default network provider.
      tileProvider: base.paramsForZoom == null
          ? null
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
    // Wind overlay (arrow markers, over the chart, under boat markers).
    if (windOn && windMarkers.isNotEmpty) MarkerLayer(markers: windMarkers),
    // Active route: line from the boat to the destination.
    if (state.position != null && state.destination != null)
      PolylineLayer(
        polylines: [
          Polyline(
            points: [state.position!, state.destination!],
            strokeWidth: 3,
            color: Colors.purpleAccent,
          ),
        ],
      ),
    if (state.destination != null)
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
            child: state.usingPhoneGps
                ? const PhoneGpsMarker()
                : BoatMarker(
                    headingDeg:
                        (state.effectiveHeadingDeg ?? 0) + rotationDeg),
          ),
        ],
      ),
  ];
}
