import 'package:flutter_map/flutter_map.dart';

import 'config.dart';

/// Base map layers, each an XYZ raster URL into the existing Go tile server
/// (or, for satellite, Esri). This is the crux of the port: the phone renders
/// nothing — it just points flutter_map's TileLayer at the same endpoints the
/// OpenLayers web app uses (src/marineMap.svelte).

/// Zoom tiers for the Checkmate chart tile params (A1). The web app's
/// tileUrlFunction (src/marineMap.svelte) switches the tile render per zoom so
/// each chart feature has exactly one source of truth: once a vector layer
/// takes over a feature class, the tile must stop baking that class in.
///
/// Mobile has no navaid/structure vector layers yet, so both tiers ship "off"
/// (null — every tile gets the overview render). B1 (navaids) flips the first
/// to 12 and B2 (structures) the second to 14, matching the web app's
/// VECTOR_TILE_NAVAID_MIN_Z / VECTOR_TILE_STRUCTURE_MIN_Z. Do not turn a tier
/// on without landing its vector layer: the features the tile stops drawing
/// would simply vanish from the chart.
const int? vectorTileNavaidMinZ = null; // B1: set to 12
const int? vectorTileStructureMinZ = null; // B2: set to 14

/// Query params for a Checkmate chart tile at tile-zoom [z] — the web app's
/// overview / mid / detail variants:
///
///   overview (z below navaid tier):    style=ecdis (everything baked in)
///   mid (navaid tier ≤ z < structure): style=wms&navaids=0
///   detail (z ≥ structure tier):       mid + skip=BRIDGE,CBLOHD,PIPOHD,CONVYR
///
/// Keys off *tile* z, like the web app's tileUrlFunction. (A5's OSM
/// suppression deliberately gates on *view* zoom instead — don't conflate
/// the two.)
String chartTileParams(
  int z, {
  int? navaidMinZ = vectorTileNavaidMinZ,
  int? structureMinZ = vectorTileStructureMinZ,
}) {
  if (structureMinZ != null && z >= structureMinZ) {
    return 'style=wms&navaids=0&skip=BRIDGE,CBLOHD,PIPOHD,CONVYR';
  }
  if (navaidMinZ != null && z >= navaidMinZ) {
    return 'style=wms&navaids=0';
  }
  return 'style=ecdis';
}

/// Full query string for a chart tile: the zoom-tiered render params (A1)
/// plus the params carried on every chart request — `v=<buildStamp>` so a
/// new release busts HTTP/tile caches (A3), and `sd=<feet>` when the
/// operator has set a safe depth, driving the DEPARE shading (A2). A null
/// safe depth omits the param entirely (never `sd=`), matching the web app.
String chartTileUrlParams(
  int z, {
  required String buildStamp,
  int? safeDepthFt,
  int? navaidMinZ = vectorTileNavaidMinZ,
  int? structureMinZ = vectorTileStructureMinZ,
}) {
  final sb = StringBuffer(
      chartTileParams(z, navaidMinZ: navaidMinZ, structureMinZ: structureMinZ))
    ..write('&v=${Uri.encodeComponent(buildStamp)}');
  if (safeDepthFt != null) sb.write('&sd=$safeDepthFt');
  return sb.toString();
}

/// A [NetworkTileProvider] whose tile URL is the layer's urlTemplate plus
/// per-tile-zoom query params — flutter_map's equivalent of the web app's
/// tileUrlFunction.
class ZoomParamsTileProvider extends NetworkTileProvider {
  ZoomParamsTileProvider(this.paramsForZoom);

  final String Function(int z) paramsForZoom;

  @override
  String getTileUrl(TileCoordinates coordinates, TileLayer options) =>
      '${super.getTileUrl(coordinates, options)}?${paramsForZoom(coordinates.z)}';
}

class TileSource {
  const TileSource(
    this.id,
    this.label,
    this.urlTemplate, {
    this.minZoom = 0,
    this.maxZoom = 19,
    this.paramsForZoom,
  });
  final String id;
  final String label;
  final String urlTemplate;
  final int minZoom;
  final int maxZoom;

  /// When set, tiles are fetched as `urlTemplate?paramsForZoom(tileZ)` via
  /// [ZoomParamsTileProvider] instead of the bare template.
  final String Function(int z)? paramsForZoom;
}

final List<TileSource> baseLayers = [
  // Checkmate — the merged ENC + OSM chart, and the web app's DEFAULT base
  // layer (the "checkmate" layer in src/marineMap.svelte). Same /noaa-enc/tile
  // endpoint, with the query params picked per tile zoom by chartTileParams.
  // (landfill=0 is gone: the server forces ENC land fill on in the OSM merge,
  // so the flag was a no-op that only forked the tile cache into a redundant
  // shard.) Like the web app, the chart is only shown at z>=7 — a lower-z
  // request would trigger a ~10s server-side overview render for a tile
  // nobody sees.
  const TileSource(
    'checkmate',
    'Checkmate',
    '${Config.tileBase}/noaa-enc/tile/{z}/{x}/{y}.png',
    minZoom: 7,
    maxZoom: 16,
    paramsForZoom: chartTileParams,
  ),
  // Plain NOAA ENC render (default WMS style, solid land fills) — a fallback if
  // the merged Checkmate tiles look off on a given tile server.
  const TileSource(
    'noaa-enc',
    'NOAA ENC',
    '${Config.tileBase}/noaa-enc/tile/{z}/{x}/{y}.png',
    maxZoom: 16,
  ),
  // OSM land underlay served by the same module.
  const TileSource(
    'osm',
    'OpenStreetMap',
    '${Config.tileBase}/noaa-enc/osm-tile/{z}/{x}/{y}.png',
  ),
  // Free Esri World Imagery aerial base (matches the web app's satellite layer).
  const TileSource(
    'satellite',
    'Satellite (aerial)',
    'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}',
  ),
];
