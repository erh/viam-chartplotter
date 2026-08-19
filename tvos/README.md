# ChartplotterTV

Native SwiftUI Apple TV thin client for the chartplotter (see
../TVOS_PLAN.md). Talks to the Go module's LAN display API for boat
data and cameras; chart tiles come from the hosted checkmate tile
server (https://nycmaps.checkmatemaps.com/).

## Run

Open `ChartplotterTV.xcodeproj` in Xcode, pick an Apple TV simulator
(or a real Apple TV — set your signing team first), Run. Or from the
command line:

```sh
xcodebuild -project ChartplotterTV.xcodeproj -scheme ChartplotterTV \
  -destination 'generic/platform=tvOS Simulator' build
```

On launch the app browses Bonjour for `_viam-chartplotter._tcp`;
with exactly one boat on the network it connects automatically.
Manual `host:port` entry is the fallback. The chosen server persists
across launches.

## Layout

- `ChartplotterClient.swift` — polls /api/state (1s), /api/route (5s)
- `ServerDiscovery.swift` — Bonjour browse + service → URL resolution
- `ChartView.swift` — MKMapView + checkmate tile overlay, boat marker,
  route line, speed-adjusted auto-zoom (web app's formula)
- `ContentView.swift` — connect flow + chart screen with data panels
