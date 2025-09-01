export interface BoatInfo {
  name: string;
  location: [number, number]; // [latitude, longitude]
  speed: number;
  heading: number; // degrees (0-360)
  mmsi?: string; // MMSI identifier
  route?: {
    destinationLongitude?: number;
    destinationLatitude?: number;
    distanceToWaypoint?: number;
    waypointClosingVelocity?: number;
  };
}