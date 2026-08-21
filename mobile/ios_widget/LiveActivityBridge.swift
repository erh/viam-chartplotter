// Runner-side bridge: the "chartplotter/route_activity" method channel
// starts/updates/ends the route Live Activity from Dart. No plugin and no
// App Group — updates go straight through ActivityKit from the app process,
// which is all a local (non-push) activity needs.
//
// Copied into ios/Runner/ by tool/ios-live-activity.sh — edit HERE
// (mobile/ios_widget/). Registration is injected into AppDelegate.swift by
// the same script.

import Flutter
import Foundation

#if canImport(ActivityKit)
import ActivityKit
#endif

class LiveActivityBridge: NSObject {
  static func register(messenger: FlutterBinaryMessenger) {
    let channel = FlutterMethodChannel(
      name: "chartplotter/route_activity",
      binaryMessenger: messenger)
    channel.setMethodCallHandler { call, result in
      guard #available(iOS 16.1, *) else {
        result(false) // too old for Live Activities — Dart goes quiet
        return
      }
      switch call.method {
      case "update":
        guard let args = call.arguments as? [String: Any] else {
          result(FlutterError(code: "args", message: "bad args", details: nil))
          return
        }
        // Await and report the outcome — a silently-swallowed failure here
        // is indistinguishable from "working" ("not showing on lock screen").
        Task {
          let status = await Self.update(args)
          DispatchQueue.main.async { result(status) }
        }
      case "end":
        Task { await Self.endAll() }
        result("ended")
      default:
        result(FlutterMethodNotImplemented)
      }
    }
  }

  @available(iOS 16.1, *)
  static func update(_ args: [String: Any]) async -> String {
    func d(_ k: String) -> Double { (args[k] as? NSNumber)?.doubleValue ?? 0 }
    let state = RouteActivityAttributes.ContentState(
      nextDistNm: d("nextDistNm"),
      nextEtaEpoch: d("nextEtaEpoch"),
      finalDistNm: d("finalDistNm"),
      finalEtaEpoch: d("finalEtaEpoch"),
      waypointCount: (args["waypointCount"] as? NSNumber)?.intValue ?? 0,
      sogKn: d("sogKn"),
      updatedEpoch: Date().timeIntervalSince1970)
    // staleDate (16.2+) makes the widget re-render once when the data ages
    // out, flipping the display to the ~estimated form.
    if let activity = Activity<RouteActivityAttributes>.activities.first {
      if #available(iOS 16.2, *) {
        await activity.update(ActivityContent(
          state: state, staleDate: Date().addingTimeInterval(90)))
      } else {
        await activity.update(using: state)
      }
      return "updated"
    }
    guard ActivityAuthorizationInfo().areActivitiesEnabled else {
      // The per-app "Live Activities" switch in Settings, or a device that
      // doesn't allow them.
      return "disabled-in-settings"
    }
    do {
      if #available(iOS 16.2, *) {
        _ = try Activity.request(
          attributes: RouteActivityAttributes(),
          content: ActivityContent(
            state: state, staleDate: Date().addingTimeInterval(90)))
      } else {
        _ = try Activity.request(
          attributes: RouteActivityAttributes(), contentState: state)
      }
      return "started"
    } catch {
      return "error: \(error)"
    }
  }

  @available(iOS 16.1, *)
  static func endAll() async {
    for a in Activity<RouteActivityAttributes>.activities {
      await a.end(dismissalPolicy: .immediate)
    }
  }
}
