import 'dart:typed_data';

import 'package:flutter/painting.dart';
import 'package:flutter_map/flutter_map.dart';

import 'us_enc_coverage.dart';

/// The canonical 1×1 transparent PNG — served in place of suppressed tiles.
final Uint8List _transparentPng = Uint8List.fromList(const [
  0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, //
  0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, //
  0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, //
  0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, //
  0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54, //
  0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, //
  0x0D, 0x0A, 0x2D, 0xB4, //
  0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
]);

/// Under-chart OSM fallback provider (A5). NOAA ENC only covers US waters,
/// so anywhere else a chart base rendered a blank screen; OSM now sits
/// underneath, and the fetch is skipped (transparent tile, no request) only
/// for tiles that fall entirely inside US ENC coverage while zoomed into the
/// chart — so OSMF's servers are only hit for foreign/uncharted waters.
///
/// The z>=7 gate MUST use the *view* zoom, not the tile's z: the map rounds
/// fractional view zooms when picking tiles, so at view zoom 6.5–7 it
/// requests z=7 tiles while the chart (minZoom 7) is still hidden — testing
/// tile z there suppressed OSM *and* had no chart: an all-white map (the web
/// app documents the same trap, src/marineMap.svelte:3549).
class OsmUnderlayTileProvider extends TileProvider {
  OsmUnderlayTileProvider({required this.viewZoom});

  /// Current fractional view zoom, read per tile request.
  final double Function() viewZoom;

  @override
  ImageProvider getImage(TileCoordinates coordinates, TileLayer options) {
    if (viewZoom() >= 7 &&
        tileFullyInUSWaters(coordinates.z, coordinates.x, coordinates.y)) {
      return MemoryImage(_transparentPng);
    }
    return NetworkImage(getTileUrl(coordinates, options), headers: headers);
  }
}
