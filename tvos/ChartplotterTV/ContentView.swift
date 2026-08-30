import SwiftUI

struct ContentView: View {
    @EnvironmentObject var client: ChartplotterClient

    var body: some View {
        if let base = client.baseURL {
            ChartScreen(baseURL: base)
        } else {
            DiscoveryView()
        }
    }
}

/// Connect flow: Bonjour list of every chartplotter on the network, with
/// manual host:port entry as the fallback. Reconnects to the remembered
/// boat as soon as it's seen online; with nothing remembered and exactly
/// one boat around, connects on its own after a short grace period.
struct DiscoveryView: View {
    @EnvironmentObject var client: ChartplotterClient
    @StateObject private var discovery = ServerDiscovery()
    @State private var manualHost = ""
    @State private var connecting = false
    @State private var autoConnectTask: Task<Void, Never>?
    @State private var probedSavedURL = false

    var body: some View {
        VStack(spacing: 32) {
            Text("Chartplotter")
                .font(.largeTitle.bold())
            if connecting {
                ProgressView("Connecting…")
            } else if discovery.servers.isEmpty {
                ProgressView("Looking for the boat on this network…")
            } else {
                VStack(spacing: 16) {
                    ForEach(discovery.servers) { server in
                        Button { connect(server) } label: {
                            HStack(spacing: 12) {
                                Text(server.name)
                                if server.name == client.savedServerName {
                                    Text("last used")
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                            }
                        }
                    }
                }
                if let saved = client.savedServerName,
                    !discovery.servers.contains(where: { $0.name == saved })
                {
                    Text("“\(saved)” is offline — it connects automatically when it appears, or pick a boat above.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            HStack(spacing: 16) {
                TextField("host:port (e.g. cm90-main1:8888)", text: $manualHost)
                    .frame(maxWidth: 700)
                Button("Connect") { connectManual() }
                    .disabled(manualHost.isEmpty)
            }
            .padding(.top, 24)
        }
        .padding(60)
        .onAppear {
            discovery.start()
            probeSavedURL()
        }
        .onDisappear {
            discovery.stop()
            autoConnectTask?.cancel()
        }
        .onChange(of: discovery.servers) { _, servers in
            autoConnect(servers)
        }
    }

    /// Zero-config rules, in priority order:
    /// 1. The remembered boat is online → connect to it, even with others
    ///    around (that's the whole point of remembering).
    /// 2. Nothing remembered and exactly one boat stays alone for a couple
    ///    of seconds → connect (the grace period lets a second boat show
    ///    up and turn this into a choice instead).
    /// A remembered boat that stays offline never silently falls through
    /// to a different one — switching boats is the user's call.
    private func autoConnect(_ servers: [DiscoveredServer]) {
        guard !connecting, client.baseURL == nil, !client.userIsChoosing else { return }
        autoConnectTask?.cancel()
        if let saved = client.savedServerName {
            if let match = servers.first(where: { $0.name == saved }) {
                connect(match)
            }
            return
        }
        guard client.savedURL == nil, servers.count == 1 else { return }
        let candidate = servers[0]
        autoConnectTask = Task {
            try? await Task.sleep(nanoseconds: 2_000_000_000)
            guard !Task.isCancelled, !connecting, client.baseURL == nil,
                discovery.servers.map(\.name) == [candidate.name]
            else { return }
            connect(candidate)
        }
    }

    /// A remembered manual URL has no Bonjour name to watch for; probe it
    /// once and reconnect only when a chartplotter actually answers there.
    private func probeSavedURL() {
        guard client.savedServerName == nil, let url = client.savedURL, !probedSavedURL else {
            return
        }
        probedSavedURL = true
        connecting = true
        Task {
            if await ChartplotterClient.isReachable(url) {
                client.connect(to: url)
            }
            connecting = false
        }
    }

    private func connect(_ server: DiscoveredServer) {
        connecting = true
        autoConnectTask?.cancel()
        Task {
            if let url = await discovery.resolveURL(for: server) {
                client.connect(to: url, rememberName: server.name)
            }
            connecting = false
        }
    }

    private func connectManual() {
        var s = manualHost
        if !s.contains("://") { s = "http://" + s }
        if let url = URL(string: s) {
            client.connect(to: url)
        }
    }
}

/// Display brightness for a salon TV: full day, dimmed dusk, and a
/// heavily-dimmed red-tinted night palette.
enum DisplayMode: String, CaseIterable {
    case day, dusk, night

    var next: DisplayMode {
        let all = Self.allCases
        return all[(all.firstIndex(of: self)! + 1) % all.count]
    }

    var icon: String {
        switch self {
        case .day: return "sun.max.fill"
        case .dusk: return "sun.horizon.fill"
        case .night: return "moon.fill"
        }
    }
}

struct ChartScreen: View {
    @EnvironmentObject var client: ChartplotterClient
    let baseURL: URL

    @State private var fullScreenCamera: CameraID?
    @State private var isPanning = false
    /// Cameras-big mode: cameras fill the screen, the chart shrinks to
    /// a corner inset. Toggled with Play/Pause; Menu also exits.
    @State private var camerasBig = false
    /// Options dialog, opened with Menu when nothing else is active.
    /// The chart map swallows every directional press for panning, so
    /// the floating buttons are unreachable by focus once the map has
    /// it — the dialog's buttons are how those actions are reached from
    /// the remote (and the simulator's arrow keys).
    @State private var showOptions = false
    @AppStorage("displayMode") private var displayModeRaw = DisplayMode.day.rawValue

    private var displayMode: DisplayMode {
        DisplayMode(rawValue: displayModeRaw) ?? .day
    }

    var body: some View {
        ZStack(alignment: .topLeading) {
            if camerasBig {
                CameraGridView(fullScreen: $fullScreenCamera)
                    .ignoresSafeArea()
            } else {
                ChartMapView(
                    baseURL: baseURL, state: client.state, route: client.route,
                    track: client.track, isPanning: $isPanning
                )
                .ignoresSafeArea()
            }
            // Night dimming sits over the chart + cameras but under the
            // panels/buttons so controls stay readable. Plain alpha
            // overlays — blend modes over UIKit-backed views are
            // unreliable on tvOS.
            if displayMode != .day {
                Color.black.opacity(displayMode == .night ? 0.6 : 0.4)
                    .ignoresSafeArea()
                    .allowsHitTesting(false)
                if displayMode == .night {
                    Color.red.opacity(0.15)
                        .ignoresSafeArea()
                        .allowsHitTesting(false)
                }
            }
            // Basic data: one tight strip along the top.
            dataBar
                .padding(24)
                .opacity(client.isOffline ? 0.5 : 1)
            // Route data: compact block on the right edge (chart mode
            // only — cameras-big gives the pixels to the cameras).
            if !camerasBig {
                VStack {
                    routePanel
                    Spacer()
                }
                .frame(maxWidth: .infinity, alignment: .trailing)
                .padding(24)
                .opacity(client.isOffline ? 0.5 : 1)
            }
            // Cameras-big: live mini-chart inset, bottom-right.
            if camerasBig {
                VStack {
                    Spacer()
                    HStack {
                        Spacer()
                        ChartMapView(
                            baseURL: baseURL, state: client.state, route: client.route,
                            track: client.track, isPanning: .constant(false)
                        )
                        .frame(width: 560, height: 315)
                        .clipShape(RoundedRectangle(cornerRadius: 14))
                        .overlay(
                            RoundedRectangle(cornerRadius: 14)
                                .stroke(.white.opacity(0.7), lineWidth: 2)
                        )
                    }
                }
                .padding(24)
            }
            if !camerasBig {
            VStack {
                Spacer()
                HStack(alignment: .bottom) {
                    CameraRow(fullScreen: $fullScreenCamera)
                    Spacer()
                    if isPanning {
                        VStack(spacing: 4) {
                            Label("Stop Panning", systemImage: "location.fill")
                                .font(.body.bold())
                            Text("press Play/Pause or Menu")
                                .font(.caption)
                                .foregroundStyle(.white.opacity(0.75))
                        }
                        .padding(.horizontal, 20)
                        .padding(.vertical, 12)
                        .background(.blue.opacity(0.85), in: RoundedRectangle(cornerRadius: 16))
                        .foregroundStyle(.white)
                    }
                    // Back to the boat chooser (e.g. several chartplotters
                    // on the network, or a mistyped manual address).
                    Button {
                        client.switchServer()
                    } label: {
                        Image(systemName: "antenna.radiowaves.left.and.right")
                            .font(.title3)
                            .padding(14)
                            .background(.black.opacity(0.55), in: Circle())
                            .foregroundStyle(.white)
                    }
                    .buttonStyle(.card)
                    Button {
                        displayModeRaw = displayMode.next.rawValue
                    } label: {
                        Image(systemName: displayMode.icon)
                            .font(.title3)
                            .padding(14)
                            .background(.black.opacity(0.55), in: Circle())
                            .foregroundStyle(.white)
                    }
                    .buttonStyle(.card)
                }
            }
            .padding(24)
            }
            if client.isOffline {
                VStack {
                    Spacer()
                    Text("Connection lost — reconnecting…")
                        .font(.title3.bold())
                        .padding(.horizontal, 24)
                        .padding(.vertical, 12)
                        .background(.red.opacity(0.85), in: Capsule())
                        .foregroundStyle(.white)
                        .padding(.bottom, 120)
                }
                .frame(maxWidth: .infinity)
                .allowsHitTesting(false)
            }
        }
        .fullScreenCover(item: $fullScreenCamera) { cam in
            FullScreenCameraView(camera: cam)
        }
        // Play/Pause: stop panning if panning, otherwise toggle
        // cameras-big mode (works regardless of focus — while the map
        // has focus, directional input pans it, so on-screen buttons
        // can be unreachable). Menu backs out of panning/cameras-big,
        // and otherwise opens the options dialog — a wall chartplotter
        // shouldn't drop to the home screen on a stray Menu press, and
        // the dialog is the focus-reachable path to the floating
        // buttons' actions. The TV/Home button still leaves the app.
        .onPlayPauseCommand {
            if isPanning {
                isPanning = false
            } else if !(client.info?.cameras ?? []).isEmpty {
                camerasBig.toggle()
            }
        }
        .onExitCommand {
            if isPanning || camerasBig {
                isPanning = false
                camerasBig = false
            } else {
                showOptions = true
            }
        }
        .confirmationDialog("Options", isPresented: $showOptions) {
            Button("Switch boat") { client.switchServer() }
            Button("Display: \(displayMode.next.rawValue)") {
                displayModeRaw = displayMode.next.rawValue
            }
            Button("Cancel", role: .cancel) {}
        }
        .onAppear {
            // A wall chartplotter must never hand the screen to the
            // screensaver.
            UIApplication.shared.isIdleTimerDisabled = true
        }
        .onDisappear {
            UIApplication.shared.isIdleTimerDisabled = false
        }
    }

    /// Horizontal strip of the basics — MFD-style label-over-value
    /// cells keep it much tighter than label/value rows.
    private var dataBar: some View {
        HStack(spacing: 0) {
            let cells = statCells
            if cells.isEmpty {
                if let err = client.lastError {
                    Text(err)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .padding(.horizontal, 16)
                } else {
                    ProgressView()
                        .padding(.horizontal, 16)
                }
            }
            ForEach(Array(cells.enumerated()), id: \.offset) { i, cell in
                if i > 0 {
                    Rectangle()
                        .fill(.white.opacity(0.15))
                        .frame(width: 1, height: 44)
                }
                statCell(cell.0, cell.1, cell.2)
            }
        }
        .padding(.vertical, 10)
        .padding(.horizontal, 6)
        .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 14))
    }

    private var statCells: [(String, String, String?)] {
        guard let s = client.state else { return [] }
        var out: [(String, String, String?)] = []
        if let sog = s.sogKn {
            out.append(("SOG", String(format: "%.1f", sog), "kn"))
        }
        if let d = s.depthFt {
            out.append(("Depth", String(format: "%.0f", d), "ft"))
        }
        if let h = s.headingDeg {
            out.append(("HDG", String(format: "%03.0f°", h), nil))
        }
        if let c = s.cogDeg {
            out.append(("COG", String(format: "%03.0f°", c), nil))
        }
        return out
    }

    private func statCell(_ label: String, _ value: String, _ unit: String?) -> some View {
        VStack(alignment: .center, spacing: 2) {
            Text(label)
                .font(.caption2)
                .textCase(.uppercase)
                .foregroundStyle(.white.opacity(0.6))
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                Text(value)
                    .font(.title3.monospacedDigit().bold())
                    .foregroundStyle(.white)
                if let unit {
                    Text(unit)
                        .font(.caption)
                        .foregroundStyle(.white.opacity(0.7))
                }
            }
        }
        .padding(.horizontal, 16)
    }

    /// One leg of the route summary: distance plus (with a usable
    /// speed) time to go.
    private struct RouteLeg {
        let nm: Double
        let seconds: Double?
    }

    /// Next-waypoint leg: the nav system's DTW + closing velocity when
    /// it has an active route, else straight-line from the boat to the
    /// first nav-service waypoint at current SOG.
    private var nextLeg: RouteLeg? {
        let sogKn = client.state?.sogKn ?? 0
        if let nm = client.route?.distanceToWaypointNM {
            return RouteLeg(nm: nm, seconds: client.route?.etaSeconds
                ?? (sogKn > 0.5 ? nm / sogKn * 3600 : nil))
        }
        guard let s = client.state, let wp = client.route?.waypoints?.first else { return nil }
        let nm = haversineNM(s.lat, s.lng, wp.lat, wp.lng)
        return RouteLeg(nm: nm, seconds: sogKn > 0.5 ? nm / sogKn * 3600 : nil)
    }

    /// Whole-route leg: the next leg plus every remaining
    /// waypoint-to-waypoint hop, timed at current SOG. Only meaningful
    /// with 2+ waypoints — with one, it's identical to nextLeg.
    private var finalLeg: RouteLeg? {
        guard let wps = client.route?.waypoints, wps.count >= 2,
            var prev = wps.first, let first = nextLeg
        else { return nil }
        var total = first.nm
        for wp in wps.dropFirst() {
            total += haversineNM(prev.lat, prev.lng, wp.lat, wp.lng)
            prev = wp
        }
        let sogKn = client.state?.sogKn ?? 0
        return RouteLeg(nm: total, seconds: sogKn > 0.5 ? total / sogKn * 3600 : nil)
    }

    @ViewBuilder
    private var routePanel: some View {
        let wpCount = client.route?.waypoints?.count ?? 0
        VStack(alignment: .trailing, spacing: 6) {
            // Wall-clock now — the panel re-renders on every 1s state
            // poll, which keeps a minutes-resolution clock current.
            row("Now", Date().formatted(date: .omitted, time: .shortened))
            if let next = nextLeg {
                panelDivider
                row("Next", String(format: "%.2f nm", next.nm))
                if let v = client.route?.closingVelocityMS, v > 0.1 {
                    row("Closing", String(format: "%.1f kn", v * 1.94384))
                }
                if let s = next.seconds {
                    row("Time", Self.formatDuration(s))
                    row("ETA", Date(timeIntervalSinceNow: s).formatted(date: .omitted, time: .shortened))
                }
                if let final = finalLeg {
                    panelDivider
                    row("Final", String(format: "%.2f nm", final.nm))
                    if let s = final.seconds {
                        row("Time", Self.formatDuration(s))
                        row("ETA", Date(timeIntervalSinceNow: s).formatted(date: .omitted, time: .shortened))
                    }
                }
                if wpCount > 1 {
                    panelDivider
                    row("WPTS", "\(wpCount)")
                }
            }
        }
        .panelStyle()
    }

    private var panelDivider: some View {
        Rectangle()
            .fill(.white.opacity(0.15))
            .frame(width: 250, height: 1)
            .padding(.vertical, 3)
    }

    private func row(_ label: String, _ value: String) -> some View {
        HStack(spacing: 12) {
            Text(label)
                .font(.caption)
                .textCase(.uppercase)
                .foregroundStyle(.white.opacity(0.6))
                .lineLimit(1)
                .fixedSize()
            Spacer(minLength: 0)
            Text(value)
                .font(.body.monospacedDigit().bold())
                .foregroundStyle(.white)
                .lineLimit(1)
                .fixedSize()
        }
        .frame(width: 250)
    }

    static func formatDuration(_ seconds: Double) -> String {
        let mins = Int((seconds / 60).rounded())
        if mins < 60 { return "\(mins) min" }
        return "\(mins / 60)h \(mins % 60)m"
    }
}

extension View {
    func panelStyle() -> some View {
        padding(16)
            .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 14))
            .foregroundStyle(.white)
    }
}
