import Map from 'ol/Map';
import View from 'ol/View';
import Collection from 'ol/Collection.js';
export interface LayerOption {
    name: string;
    on: boolean;
    layer: any;
}
export interface MapGlobal {
    map: Map | null;
    view: View | null;
    aisFeatures: Collection<any>;
    trackFeatures: Collection<any>;
    routeFeatures: Collection<any>;
    trackFeaturesLastCheck: Date;
    myBoatMarker: any;
    inPanMode: boolean;
    lastZoom: number;
    lastCenter: number[] | null;
    layerOptions: LayerOption[];
    onLayers: Collection<any>;
}
/**
 * Setup all layer options with their configurations
 */
export declare function setupLayers(aisFeatures: Collection<any>, trackFeatures: Collection<any>, routeFeatures: Collection<any>, boatImage: string, getGlobalDataHeading: () => number): LayerOption[];
/**
 * Find layer option by name
 */
export declare function findLayerByName(layerOptions: LayerOption[], name: string): LayerOption | null;
/**
 * Find index of layer in onLayers collection by name
 */
export declare function findOnLayerIndexOfName(layerOptions: LayerOption[], onLayers: Collection<any>, name: string): number;
/**
 * Update onLayers collection based on layer options
 */
export declare function updateOnLayers(layerOptions: LayerOption[], onLayers: Collection<any>): void;
