export interface PositionPoint {
  lat: number;
  lng: number;
  ts: Date;
}

export interface Detection {
  id: string;
  timestamp: Date;
  boatId: string;
  metadata?: Record<string, any>;
}

export interface DetectionConfig {
  detections?: Detection[];
  enabled?: boolean;
  loading?: boolean;
  onToggle?: (enabled: boolean, boatPartId?: string) => void;
  onClick?: (detection: Detection) => void;
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
  positionHistory?: PositionPoint[];
}