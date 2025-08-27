import Feature from 'ol/Feature.js';
import Point from 'ol/geom/Point.js';
import Circle from 'ol/geom/Circle.js';
import LineString from 'ol/geom/LineString.js';
import Collection from 'ol/Collection.js';

/**
 * Create a boat marker feature at specified coordinates
 */
export function createBoatMarker(coordinates: number[] = [0, 0]) {
  return new Feature({
    type: 'geoMarker',
    header: 0,
    geometry: new Point(coordinates),
  });
}

/**
 * Create AIS feature for vessel tracking
 */
export function createAISFeature(mmsi: string, boat: any) {
  return new Feature({
    type: "ais",
    mmsi: mmsi,
    heading: boat.Heading,
    geometry: new Point([boat.Location[1], boat.Location[0]]),
  });
}

/**
 * Create track point feature
 */
export function createTrackPoint(coordinates: number[]) {
  return new Feature({
    type: "track",
    geometry: new Circle(coordinates)
  });
}

/**
 * Create track line feature connecting two points
 */
export function createTrackLine(startCoords: number[], endCoords: number[]) {
  return new Feature({
    type: "track",
    geometry: new LineString([startCoords, endCoords])
  });
}

/**
 * Process NMEA 2000 PGN 129285
 * Clears existing route features and creates new ones from waypoint list
 */
export function processRoute129285(doc: any, routeFeatures: Collection<Feature>) {
  console.log(doc);
  
  routeFeatures.clear();

  if (!doc.list) {
    return;
  }
  
  let prev: number[] = [];
  
  for (let x = 0; x < doc.list.length; x++) {
    const wp = doc.list[x];
    const loc = [wp["WP Longitude"], wp["WP Latitude"]];
    if (prev.length > 0) {
      const f = new Feature({
        type: "track",
        geometry: new LineString([prev, loc]),
      });
      routeFeatures.push(f);
    }
    prev = loc;
  }
  
  console.log(routeFeatures);
}

/**
 * Update AIS features collection with new vessel data
 * Removes stale vessels and updates/adds current ones
 */
export function updateAISFeatures(aisFeatures: Collection<Feature>, rawAISData: any) {
  const good: Record<string, boolean> = {};
  
  for (const mmsi in rawAISData) {
    const boat = rawAISData[mmsi];
    
    if (boat == null || boat.Location == null || boat.Location.length != 2 || boat.Location[0] == null) {
      continue;
    }
    
    good[mmsi] = true;
    
    let found = false;
    
    for (let i = 0; i < aisFeatures.getLength(); i++) {
      const v = aisFeatures.item(i);
      if (v.get("mmsi") == mmsi) {
        found = true;
        v.setGeometry(new Point([boat.Location[1], boat.Location[0]]));
        break;
      }
    }
    
    if (!found) {
      aisFeatures.push(createAISFeature(mmsi, boat));
    }
  }
  
  // Remove stale AIS features
  for (let i = 0; i < aisFeatures.getLength(); i++) {
    const v = aisFeatures.item(i);
    const mmsi = v.get("mmsi");
    if (!good[mmsi]) {
      aisFeatures.removeAt(i);
      i--; // Adjust index after removal
    }
  }
}