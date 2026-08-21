// Live Activity payload for the active route (lock screen + Dynamic
// Island). Compiled into BOTH the Runner app (which starts/updates the
// activity over the "chartplotter/route_activity" method channel) and the
// RouteActivity widget extension (which renders it) — the struct must stay
// byte-identical between them, which is why it lives in one shared file.
//
// NOTE: this file is copied into the regenerated ios/ project by
// tool/ios-live-activity.sh — edit it HERE (mobile/ios_widget/), never in
// ios/, or the change is lost on the next flutter create.

import Foundation

#if canImport(ActivityKit)
import ActivityKit

@available(iOS 16.1, *)
struct RouteActivityAttributes: ActivityAttributes {
  public struct ContentState: Codable, Hashable {
    /// Distance to the next waypoint, nautical miles.
    var nextDistNm: Double
    /// ETA at the next waypoint, seconds since epoch; 0 = unknown
    /// (stationary — the UI blanks rather than showing zero).
    var nextEtaEpoch: Double
    /// Whole remaining route, nautical miles.
    var finalDistNm: Double
    var finalEtaEpoch: Double
    var waypointCount: Int
    /// Speed over ground, knots, at push time — the widget dead-reckons an
    /// estimated remaining distance from it once the data goes stale
    /// (remaining time × SOG), shown with a leading "~".
    var sogKn: Double
    /// When the app last pushed real numbers — distances freeze while the
    /// phone is locked (iOS suspends the app); the countdowns keep ticking.
    var updatedEpoch: Double
  }
}
#endif
