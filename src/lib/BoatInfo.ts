export interface PositionPoint {
  lat: number;
  lng: number;
  ts: Date;
  depth?: number;
}

export interface Detection {
  id: string;
  timestamp: Date;
  boatId: string;
  metadata?: Record<string, any>;
}

export interface DetectionConfig {
  onClick?: (detection: Detection) => void;
}

export interface BoatInfo {
  name: string;
  location: [number, number]; // [latitude, longitude]
  speed: number;
  heading: number; // degrees (0-360)
  cog?: number; // course over ground (degrees) — preferred over heading for CPA
  length?: number; // overall length in meters (AIS static data, type 5)
  beam?: number; // overall beam/width in meters (AIS static data, type 5)
  destination?: string; // free-text destination (AIS static data, type 5)
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
  positionHistory?: PositionPoint[];
}
