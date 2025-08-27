import Feature from 'ol/Feature.js';
import Point from 'ol/geom/Point.js';
import Circle from 'ol/geom/Circle.js';
import LineString from 'ol/geom/LineString.js';
import Collection from 'ol/Collection.js';
/**
 * Create a boat marker feature at specified coordinates
 */
export declare function createBoatMarker(coordinates?: number[]): Feature<Point>;
/**
 * Create AIS feature for vessel tracking
 */
export declare function createAISFeature(mmsi: string, boat: any): Feature<Point>;
/**
 * Create track point feature
 */
export declare function createTrackPoint(coordinates: number[]): Feature<Circle>;
/**
 * Create track line feature connecting two points
 */
export declare function createTrackLine(startCoords: number[], endCoords: number[]): Feature<LineString>;
/**
 * Process NMEA 2000 PGN 129285
 * Clears existing route features and creates new ones from waypoint list
 */
export declare function processRoute129285(doc: any, routeFeatures: Collection<Feature>): void;
/**
 * Update AIS features collection with new vessel data
 * Removes stale vessels and updates/adds current ones
 */
export declare function updateAISFeatures(aisFeatures: Collection<Feature>, rawAISData: any): void;
