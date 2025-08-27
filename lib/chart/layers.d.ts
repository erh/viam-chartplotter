import TileLayer from 'ol/layer/Tile';
import TileWMS from 'ol/source/TileWMS.js';
import XYZ from 'ol/source/XYZ';
/**
 * Custom tile URL function (for seamark tiles)
 * Handles coordinate wrapping and URL selection
 */
export declare function getTileUrlFunction(url: string | string[], type: string, coordinates: number[]): string;
/**
 * Create OpenStreetMap tile layer
 */
export declare function createOSMLayer(): TileLayer<XYZ>;
/**
 * Create depth/bathymetry layer from OpenSeaMap
 */
export declare function createDepthLayer(): TileLayer<TileWMS>;
/**
 * Create seamark layer from OpenSeaMap
 */
export declare function createSeamarkLayer(): TileLayer<XYZ>;
/**
 * Create NOAA chart layer
 */
export declare function createNOAALayer(): TileLayer<TileWMS>;
