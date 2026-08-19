import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';

import '../ais.dart';
import '../boat_state.dart';
import '../tile_sources.dart';
import 'boat_marker.dart';

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
  required void Function(AisBoat) onAisTap,
}) {
  return [
    TileLayer(
      key: ValueKey(base.id),
      urlTemplate: base.urlTemplate,
      // Chart tiles vary their query params by tile zoom (A1); other layers
      // use the plain template with the default network provider.
      tileProvider: base.paramsForZoom == null
          ? null
          : ZoomParamsTileProvider(base.paramsForZoom!),
      minZoom: base.minZoom.toDouble(),
      maxNativeZoom: base.maxZoom,
      userAgentPackageName: 'com.viam.chartplotter.mobile',
      // flutter_map shows nothing for a failed tile, so a broken URL/host
      // reads as a blank map — log it instead.
      errorTileCallback: (tile, error, stackTrace) =>
          debugPrint('tile load failed (${base.id}): $error'),
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
    // AIS targets (drawn under the own-boat marker).
    if (state.aisBoats.isNotEmpty)
      MarkerLayer(
        markers: [
          for (final b in state.aisBoats)
            Marker(
              point: b.location,
              width: 30,
              height: 30,
              child: GestureDetector(
                onTap: () => onAisTap(b),
                child: Transform.rotate(
                  angle: (b.orientationDeg + rotationDeg) * math.pi / 180.0,
                  child: const Icon(Icons.navigation,
                      color: Colors.cyanAccent, size: 22),
                ),
              ),
            ),
        ],
      ),
    if (state.position != null)
      MarkerLayer(
        markers: [
          Marker(
            point: state.position!,
            width: 40,
            height: 40,
            child:
                BoatMarker(headingDeg: (state.headingDeg ?? 0) + rotationDeg),
          ),
        ],
      ),
  ];
}
