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
      LockScreenView(state: context.state)
        .padding(14)
        .activityBackgroundTint(Color.black.opacity(0.8))
        .activitySystemActionForegroundColor(.white)
    } dynamicIsland: { context in
      DynamicIsland {
        DynamicIslandExpandedRegion(.leading) {
          NextColumn(state: context.state)
        }
        DynamicIslandExpandedRegion(.trailing) {
          FinalColumn(state: context.state)
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

struct NextColumn: View {
  let state: RouteActivityAttributes.ContentState
  var body: some View {
    VStack(alignment: .leading, spacing: 2) {
      Text("NEXT").font(.caption2).foregroundStyle(.secondary)
      Text(nm(state.nextDistNm)).font(.title3).bold().monospacedDigit()
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
  var body: some View {
    VStack(alignment: .trailing, spacing: 2) {
      Text("FINAL · \(state.waypointCount) WP")
        .font(.caption2).foregroundStyle(.secondary)
      Text(nm(state.finalDistNm)).font(.title3).bold().monospacedDigit()
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
  var body: some View {
    VStack(spacing: 6) {
      HStack(alignment: .top) {
        NextColumn(state: state)
        Spacer()
        FinalColumn(state: state)
      }
      // Distances freeze while the phone is locked (the app is suspended);
      // say when they were last real instead of letting them lie.
      HStack {
        Image(systemName: "sailboat.fill").font(.caption2)
        Text("as of").font(.caption2)
        Text(Date(timeIntervalSince1970: state.updatedEpoch), style: .time)
          .font(.caption2).monospacedDigit()
        Spacer()
      }
      .foregroundStyle(.secondary)
    }
  }
}
