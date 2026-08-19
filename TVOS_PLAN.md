# tvOS Thin Client Plan

Goal: a native SwiftUI Apple TV app for the salon TV showing (1) the chart
with our position and speed-adjusted zoom, (2) cameras, (3) route info from
the nav system. LAN-only ("Option A"): the TV talks exclusively to the Go
module's existing HTTP server — no Viam cloud, no login, no WebRTC.

## Phase 1 — Module HTTP API (this repo, Go) — DONE

Implemented in `displayapi.go` + `module.go` (config attrs
`movement_sensor`, `depth_sensor`, `route_sensor`, `nav_service`,
`cameras` on the chartplotter resource; endpoints + mDNS below).
See README "display API" section for the endpoint reference.

The chartplotter HTTP server already serves tiles + the web app on :8888.
Add a small display-client API next to it.

1. Config: new optional attributes on the chartplotter resource —
   `movement_sensor`, `nav_service`, `cameras` (list of camera names).
   Each named resource becomes a declared dependency.
2. Endpoints (all JSON unless noted, all unauthenticated like the rest
   of the server — same LAN trust model as the web app):
   - `GET /api/info` — module version + which of the optional deps are
     configured. Doubles as the discovery/health ping.
   - `GET /api/state` — `{lat, lng, heading, cog, sog_kn, depth_ft, ts}`
     from the movement sensor. Client polls at 1 s; SSE only if polling
     ever feels laggy.
   - `GET /api/route` — active waypoint list plus distance-to-waypoint,
     closing velocity, ETA (the nav service in this same binary already
     holds all of it).
   - `GET /api/camera/{name}.jpg` — latest frame from the named camera.
     Still-frame polling, mirroring what the web app does with GetImages;
     no MJPEG/video stack in v1.
3. Discovery: advertise `_viam-chartplotter._tcp` on the server port via
   mDNS/Bonjour so the TV finds the boat with zero typing.
4. Verify with curl from another machine on the boat LAN.

## Phase 2 — tvOS app skeleton (new `tvos/` directory, Swift/SwiftUI)

1. Project scaffold + connection screen: Bonjour browse for
   `_viam-chartplotter._tcp`, auto-connect when exactly one server is
   found; manual host:port entry as fallback, remembered across launches.
2. Chart screen: `MKMapView` with an `MKTileOverlay` whose URL template
   points at the module's tile endpoints; boat marker rotated to heading.
3. Auto-follow + speed zoom: recenter on each `/api/state` tick and set
   zoom with the web app's formula (`floor(sog)^0.41`, marineMap.svelte
   ~1220), mapped onto MapKit camera altitude.

## Phase 3 — Cameras + route panel

1. Camera grid polling `/api/camera/*.jpg` every ~2 s; click/remote
   select to go full screen, Menu to return.
2. Route panel overlaid on the chart: next waypoint, DTW, ETA; draw the
   route line from `/api/route` waypoints as a map overlay.

## Phase 4 — Polish

- Reconnect/offline handling (grey the panels, keep last chart view).
- Prevent the screensaver while displaying (`isIdleTimerDisabled`).
- Day/night palette toggle.
- App icon / top shelf image.

## Non-goals (v1)

- Any interaction beyond view switching (no waypoint editing, no AIS
  popups). It's a glanceable display.
- Off-boat/cloud access, auth, WebRTC video.
- AIS targets on the chart — natural v2 once `/api/state` exists
  (module already polls AIS history for the web app).
