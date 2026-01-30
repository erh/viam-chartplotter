/** A single position point with optional timestamp */
export interface PositionPoint {
  lat: number;
  lng: number;
  ts?: Date; // Timestamp for this position
}

/** A detection event to be displayed on the map */
export interface Detection {
  id: string; // Unique identifier for this detection
  timestamp: Date; // When the detection occurred
  boatId?: string; // The boat/partId associated with this detection
  metadata?: Record<string, any>; // Application-specific metadata
}

export interface BoatInfo {
  name: string;
  location: [number, number]; // [latitude, longitude]
  speed: number;
  heading: number; // degrees (0-360)
  mmsi?: string; // MMSI identifier
  host?: string; // Viam machine host
  partId?: string; // Viam machine part ID
  isOnline?: boolean; // Whether boat is currently online
  route?: {
    destinationLongitude?: number;
    destinationLatitude?: number;
    distanceToWaypoint?: number;
    waypointClosingVelocity?: number;
  };
  positionHistory?: PositionPoint[]; // Historical track positions with optional timestamps
}