import Collection from 'ol/Collection.js';
import VectorSource from 'ol/source/Vector.js';
import { Vector } from 'ol/layer.js';
import { Icon, Style } from 'ol/style.js';
import { Fill, Stroke } from 'ol/style.js';
import { createOSMLayer, createDepthLayer, createSeamarkLayer, createNOAALayer } from './layers.js';
import { createBoatMarker } from './features.js';
/**
 * Setup all layer options with their configurations
 */
export function setupLayers(aisFeatures, trackFeatures, routeFeatures, boatImage, getGlobalDataHeading) {
    const layerOptions = [];
    // Core open street maps
    layerOptions.push({
        name: "open street map",
        on: false,
        layer: createOSMLayer(),
    });
    // Depth data
    layerOptions.push({
        name: "depth",
        on: false,
        layer: createDepthLayer(),
    });
    // Harbors/seamarks
    layerOptions.push({
        name: "seamark",
        on: false,
        layer: createSeamarkLayer(),
    });
    // NOAA charts
    layerOptions.push({
        name: "noaa",
        on: true,
        layer: createNOAALayer(),
    });
    // Boat marker layer
    const myBoatMarker = createBoatMarker();
    const myBoatFeatures = new Collection();
    myBoatFeatures.push(myBoatMarker);
    const myBoatLayer = new Vector({
        source: new VectorSource({
            features: myBoatFeatures,
        }),
        style: function (feature) {
            const scale = 0.6;
            const rotation = (getGlobalDataHeading() / 360) * Math.PI * 2;
            return new Style({
                image: new Icon({
                    src: boatImage,
                    scale: scale,
                    rotation: rotation,
                }),
            });
        },
    });
    layerOptions.push({
        name: "boat",
        on: true,
        layer: myBoatLayer,
    });
    // AIS layer
    const aisLayer = new Vector({
        source: new VectorSource({
            features: aisFeatures,
        }),
        style: function (feature) {
            const scale = 0.25;
            let rotation = 0;
            const h = feature.get("heading");
            if (h >= 0 && h < 360) {
                rotation = (h / 360) * Math.PI * 2;
            }
            return new Style({
                image: new Icon({
                    src: boatImage,
                    scale: scale,
                    rotation: rotation,
                }),
            });
        },
    });
    layerOptions.push({
        name: "ais",
        on: true,
        layer: aisLayer,
    });
    // Track layer
    const trackLayer = new Vector({
        source: new VectorSource({
            features: trackFeatures,
        }),
        style: new Style({
            stroke: new Stroke({
                color: "blue",
                width: 3
            }),
            fill: new Fill({
                color: "rgba(0, 255, 0, 0.1)"
            })
        }),
    });
    layerOptions.push({
        name: "track",
        on: true,
        layer: trackLayer,
    });
    // Route layer
    const routeLayer = new Vector({
        source: new VectorSource({
            features: routeFeatures,
        }),
        style: new Style({
            stroke: new Stroke({
                color: "green",
                width: 3
            }),
            fill: new Fill({
                color: "rgba(0, 255, 0, 0.1)"
            })
        }),
    });
    layerOptions.push({
        name: "route",
        on: true,
        layer: routeLayer,
    });
    return layerOptions;
}
/**
 * Find layer option by name
 */
export function findLayerByName(layerOptions, name) {
    for (const l of layerOptions) {
        if (l.name === name) {
            return l;
        }
    }
    return null;
}
/**
 * Find index of layer in onLayers collection by name
 */
export function findOnLayerIndexOfName(layerOptions, onLayers, name) {
    const l = findLayerByName(layerOptions, name);
    if (l == null) {
        return -2;
    }
    for (let i = 0; i < onLayers.getLength(); i++) {
        if (onLayers.item(i).ol_uid == l.layer.ol_uid) {
            return i;
        }
    }
    return -1;
}
/**
 * Update onLayers collection based on layer options
 */
export function updateOnLayers(layerOptions, onLayers) {
    for (const l of layerOptions) {
        const idx = findOnLayerIndexOfName(layerOptions, onLayers, l.name);
        if (l.on) {
            if (idx < 0) {
                onLayers.push(l.layer);
            }
        }
        else {
            if (idx >= 0) {
                onLayers.removeAt(idx);
            }
        }
    }
}
