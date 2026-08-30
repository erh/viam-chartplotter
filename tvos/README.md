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

On launch the app browses Bonjour for `_viam-chartplotter._tcp` and
lists every chartplotter it finds (a machine can advertise several —
the module suffixes the resource name onto the hostname to keep
instance names unique). The remembered boat reconnects automatically
as soon as it's seen online; with nothing remembered and exactly one
boat on the network, it connects by itself after a ~2 s grace period
(long enough for a second boat to show up and turn it into a choice).
A remembered boat that's offline shows a hint and waits — it never
silently falls through to a different boat. Manual `host:port` entry
is the fallback; manual choices are re-probed on launch rather than
blindly trusted. Menu (Esc in the simulator) opens the options
dialog — Switch boat, display mode — which is also the only
focus-reachable path to those actions, since the chart map consumes
every directional press for panning. Going back to the chooser is
deliberate: it then waits for a pick instead of auto-connecting.

## Layout

- `ChartplotterClient.swift` — polls /api/state (1s), /api/route (5s)
- `ServerDiscovery.swift` — Bonjour browse + service → URL resolution
- `ChartView.swift` — MKMapView + checkmate tile overlay, boat marker,
  route line, speed-adjusted auto-zoom (web app's formula)
- `ContentView.swift` — connect flow + chart screen with data panels
