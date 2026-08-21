// Lock-screen / Dynamic Island rendering for the active-route Live
// Activity: distance + live ETA countdown to the next waypoint, and the
// final-destination pair when the route has more than one waypoint.
//
// Copied into ios/RouteActivity/ by tool/ios-live-activity.sh — edit HERE.

import ActivityKit
import SwiftUI
import WidgetKit

@main
struct RouteActivityBundle: WidgetBundle {
  var body: some Widget {
    RouteActivityWidget()
  }
}

struct RouteActivityWidget: Widget {
  var body: some WidgetConfiguration {
    ActivityConfiguration(for: RouteActivityAttributes.self) { context in
      LockScreenView(state: context.state, stale: context.isStale)
        .padding(14)
        .activityBackgroundTint(Color.black.opacity(0.8))
        .activitySystemActionForegroundColor(.white)
    } dynamicIsland: { context in
      DynamicIsland {
        DynamicIslandExpandedRegion(.leading) {
          NextColumn(state: context.state, stale: context.isStale)
        }
        DynamicIslandExpandedRegion(.trailing) {
          FinalColumn(state: context.state, stale: context.isStale)
        }
      } compactLeading: {
        Image(systemName: "location.north.line.fill")
          .foregroundStyle(.cyan)
      } compactTrailing: {
        if let eta = futureDate(context.state.nextEtaEpoch) {
          Text(timerInterval: Date.now...eta, countsDown: true)
            .monospacedDigit()
            .frame(maxWidth: 60)
        } else {
          Text(nm(context.state.nextDistNm)).monospacedDigit()
        }
      } minimal: {
        Image(systemName: "location.north.line.fill")
          .foregroundStyle(.cyan)
      }
    }
  }
}

private func nm(_ v: Double) -> String {
  String(format: "%.2f nm", v)
}

/// A future Date for an epoch, or nil when unknown/past — a countdown to a
/// past instant renders as counting UP, which reads as nonsense.
private func futureDate(_ epoch: Double) -> Date? {
  guard epoch > 0 else { return nil }
  let d = Date(timeIntervalSince1970: epoch)
  return d > Date.now ? d : nil
}

/// Distance text: exact while the data is fresh; once stale, dead-reckon
/// from the last SOG (remaining time to the pushed ETA × speed) and prefix
/// "~" so a frozen-app number never masquerades as live. Falls back to
/// ~last-known when there is nothing to reckon with.
private func distText(_ dist: Double, etaEpoch: Double, sog: Double,
                      stale: Bool) -> String {
  guard stale else { return nm(dist) }
  if sog > 0.3, let eta = futureDate(etaEpoch) {
    let est = eta.timeIntervalSince(Date.now) / 3600.0 * sog
    return "~" + nm(max(0, min(dist, est)))
  }
  return "~" + nm(dist)
}

struct NextColumn: View {
  let state: RouteActivityAttributes.ContentState
  var stale: Bool = false
  var body: some View {
    VStack(alignment: .leading, spacing: 2) {
      Text("NEXT").font(.caption2).foregroundStyle(.secondary)
      Text(distText(state.nextDistNm, etaEpoch: state.nextEtaEpoch,
                    sog: state.sogKn, stale: stale))
        .font(.title3).bold().monospacedDigit()
      if let eta = futureDate(state.nextEtaEpoch) {
        Text(timerInterval: Date.now...eta, countsDown: true)
          .font(.caption).monospacedDigit().foregroundStyle(.cyan)
      } else {
        Text("—").font(.caption).foregroundStyle(.secondary)
      }
    }
  }
}

struct FinalColumn: View {
  let state: RouteActivityAttributes.ContentState
  var stale: Bool = false
  var body: some View {
    VStack(alignment: .trailing, spacing: 2) {
      Text("FINAL · \(state.waypointCount) WP")
        .font(.caption2).foregroundStyle(.secondary)
      Text(distText(state.finalDistNm, etaEpoch: state.finalEtaEpoch,
                    sog: state.sogKn, stale: stale))
        .font(.title3).bold().monospacedDigit()
      if let eta = futureDate(state.finalEtaEpoch) {
        Text(Date(timeIntervalSince1970: state.finalEtaEpoch), style: .time)
          .font(.caption).monospacedDigit().foregroundStyle(.cyan)
      } else {
        Text("—").font(.caption).foregroundStyle(.secondary)
      }
    }
  }
}

struct LockScreenView: View {
  let state: RouteActivityAttributes.ContentState
  var stale: Bool = false
  var body: some View {
    VStack(spacing: 6) {
      HStack(alignment: .top) {
        NextColumn(state: state, stale: stale)
        Spacer()
        FinalColumn(state: state, stale: stale)
      }
      // The age of the data, ticking live (relative-style Text is rendered
      // by the system) — "~" distances are estimates based on data from
      // exactly this long ago.
      HStack(spacing: 4) {
        Image(systemName: "sailboat.fill").font(.caption2)
        Text(stale ? "estimated — data from" : "data from").font(.caption2)
        Text(Date(timeIntervalSince1970: state.updatedEpoch), style: .relative)
          .font(.caption2).monospacedDigit()
        Text("ago").font(.caption2)
        Spacer()
      }
      .foregroundStyle(.secondary)
    }
  }
}
