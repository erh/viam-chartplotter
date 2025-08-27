import TileLayer from 'ol/layer/Tile';
import TileWMS from 'ol/source/TileWMS.js';
import XYZ from 'ol/source/XYZ';

/**
 * Custom tile URL function (for seamark tiles)
 * Handles coordinate wrapping and URL selection
 */
export function getTileUrlFunction(url: string | string[], type: string, coordinates: number[]) {
  const x = coordinates[1];
  let y = coordinates[2];
  const z = coordinates[0];
  const limit = Math.pow(2, z);
  if (y < 0 || y >= limit) {
    return null;
  } else {
    const wrappedX = ((x % limit) + limit) % limit;
    
    const path = z + "/" + wrappedX + "/" + y + "." + type;
    if (url instanceof Array) {
      url = url[0];
    }
    return url + path;
  }
}

/**
 * Create OpenStreetMap tile layer
 */
export function createOSMLayer() {
  return new TileLayer({
    opacity: .5,
    source: new XYZ({
      url: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png'
    })
  });
}

/**
 * Create depth/bathymetry layer from OpenSeaMap
 */
export function createDepthLayer() {
  return new TileLayer({
    opacity: .7,
    source: new TileWMS({
      url: 'https://geoserver.openseamap.org/geoserver/gwc/service/wms',
      params: {'LAYERS': 'gebco2021:gebco_2021', 'VERSION':'1.1.1'},
      serverType: 'geoserver',
      hidpi: false,
    }),
  });
}

/**
 * Create seamark layer from OpenSeaMap
 */
export function createSeamarkLayer() {
  return new TileLayer({
    visible: true,
    maxZoom: 19,
    source: new XYZ({
      tileUrlFunction: function(coordinate) {
        return getTileUrlFunction("https://tiles.openseamap.org/seamark/", 'png', coordinate);
      }
    }),
    properties: {
      name: "seamarks",
      layerId: 3,
      cookieKey: "SeamarkLayerVisible",
      checkboxId: "checkLayerSeamark",
    }
  });
}

/**
 * Create NOAA chart layer
 */
export function createNOAALayer() {
  return new TileLayer({
    opacity: .7,
    source: new TileWMS({
      url: "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer",
      params: {}
    }),
  });
}