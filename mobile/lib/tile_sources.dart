import 'config.dart';

/// Base map layers, each a straight XYZ raster URL into the existing Go
/// tile server (or, for satellite, Esri). This is the crux of the port:
/// the phone renders nothing — it just points flutter_map's TileLayer at the
/// same endpoints the OpenLayers web app uses (src/marineMap.svelte).
class TileSource {
  const TileSource(
    this.id,
    this.label,
    this.urlTemplate, {
    this.minZoom = 0,
    this.maxZoom = 19,
  });
  final String id;
  final String label;
  final String urlTemplate;
  final int minZoom;
  final int maxZoom;
}

final List<TileSource> baseLayers = [
  // Checkmate — the merged ENC + OSM chart, and the web app's DEFAULT base
  // layer (the "checkmate" layer in src/marineMap.svelte). Same /noaa-enc/tile
  // endpoint, but with the overview render params the web app uses: ECDIS style
  // + landfill=0 so the composited OSM land shows through. Navaids/structures
  // are baked into the tile (mobile has no interactive vector overlays yet).
  // Like the web app, the chart is only shown at z>=7.
  const TileSource(
    'checkmate',
    'Checkmate',
    '${Config.tileBase}/noaa-enc/tile/{z}/{x}/{y}.png?style=ecdis&landfill=0',
    minZoom: 7,
    maxZoom: 16,
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
