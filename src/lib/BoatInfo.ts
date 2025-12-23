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
  positionHistory?: { lat: number; lng: number }[]; // Historical track positions
}