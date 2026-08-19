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

/// Great-circle distance in nautical miles (haversine).
func haversineNM(_ lat1: Double, _ lng1: Double, _ lat2: Double, _ lng2: Double) -> Double {
    let r = 3440.065 // earth radius, nm
    let dLat = (lat2 - lat1) * .pi / 180
    let dLng = (lng2 - lng1) * .pi / 180
    let a =
        sin(dLat / 2) * sin(dLat / 2)
        + cos(lat1 * .pi / 180) * cos(lat2 * .pi / 180) * sin(dLng / 2) * sin(dLng / 2)
    return r * 2 * atan2(sqrt(a), sqrt(1 - a))
}
