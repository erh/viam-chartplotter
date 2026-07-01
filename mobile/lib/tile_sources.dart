import 'config.dart';

/// Base map layers, each a straight XYZ raster URL into the existing Go
/// tile server (or, for satellite, Esri). This is the crux of the port:
/// the phone renders nothing — it just points flutter_map's TileLayer at the
/// same endpoints the OpenLayers web app uses (src/marineMap.svelte).
class TileSource {
  const TileSource(this.id, this.label, this.urlTemplate, {this.maxZoom = 19});
  final String id;
  final String label;
  final String urlTemplate;
  final int maxZoom;
}

final List<TileSource> baseLayers = [
  // NOAA ENC vector charts rendered to PNG server-side, depth-shaded by the
  // configured draft. The `?` style params from the web app can be appended
  // here later; default style is fine for the spike.
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
