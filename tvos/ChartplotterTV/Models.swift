import Foundation

// Wire types for the Go module's display API (displayapi.go).
// Decoded with .convertFromSnakeCase, so `sog_kn` → `sogKn`, etc.

struct ServerInfo: Decodable {
    let state: Bool
    let depth: Bool
    let route: Bool
    let nav: Bool
    let cameras: [String]
}

struct BoatState: Decodable {
    let lat: Double
    let lng: Double
    let sogKn: Double?
    let headingDeg: Double?
    let cogDeg: Double?
    let depthFt: Double?
    let ts: Double
}

struct TrackPoint: Decodable, Equatable {
    let lat: Double
    let lng: Double
    let ts: Double
}

struct TrackResponse: Decodable {
    let points: [TrackPoint]
}

struct Waypoint: Decodable, Identifiable, Equatable {
    let id: String
    let lat: Double
    let lng: Double
}

struct RouteInfo: Decodable, Equatable {
    let destinationLat: Double?
    let destinationLng: Double?
    let distanceToWaypointM: Double?
    let closingVelocityMS: Double?
    let waypoints: [Waypoint]?

    var distanceToWaypointNM: Double? {
        distanceToWaypointM.map { $0 / 1852.0 }
    }

    /// Seconds to the active waypoint, when the boat is actually closing.
    var etaSeconds: Double? {
        guard let d = distanceToWaypointM, let v = closingVelocityMS, v > 0.1 else { return nil }
        return d / v
    }
}
