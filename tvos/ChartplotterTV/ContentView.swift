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

/// Connect flow: Bonjour list (auto-connects when exactly one boat is
/// found), with manual host:port entry as the fallback.
struct DiscoveryView: View {
    @EnvironmentObject var client: ChartplotterClient
    @StateObject private var discovery = ServerDiscovery()
    @State private var manualHost = ""
    @State private var connecting = false
    @State private var autoConnectTried = false

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
                        Button(server.name) { connect(server) }
                    }
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
        .onAppear { discovery.start() }
        .onDisappear { discovery.stop() }
        .onChange(of: discovery.servers) { _, servers in
            // Zero-config path: exactly one boat on the network → just go.
            if servers.count == 1, !autoConnectTried, !connecting {
                autoConnectTried = true
                connect(servers[0])
            }
        }
    }

    private func connect(_ server: DiscoveredServer) {
        connecting = true
        Task {
            if let url = await discovery.resolveURL(for: server) {
                client.connect(to: url)
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

struct ChartScreen: View {
    @EnvironmentObject var client: ChartplotterClient
    let baseURL: URL

    var body: some View {
        ZStack(alignment: .top) {
            ChartMapView(baseURL: baseURL, state: client.state, route: client.route)
                .ignoresSafeArea()
            HStack(alignment: .top) {
                dataPanel
                Spacer()
                routePanel
            }
            .padding(40)
        }
    }

    private var dataPanel: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let s = client.state {
                if let sog = s.sogKn {
                    row("SOG", String(format: "%.1f kn", sog))
                }
                if let d = s.depthFt {
                    row("Depth", String(format: "%.0f ft", d))
                }
                if let h = s.headingDeg {
                    row("HDG", String(format: "%03.0f°", h))
                }
                if let c = s.cogDeg {
                    row("COG", String(format: "%03.0f°", c))
                }
            } else if let err = client.lastError {
                Text(err)
                    .font(.caption)
                    .foregroundStyle(.red)
            } else {
                ProgressView()
            }
        }
        .panelStyle()
    }

    @ViewBuilder
    private var routePanel: some View {
        if let r = client.route, r.distanceToWaypointNM != nil || !(r.waypoints?.isEmpty ?? true) {
            VStack(alignment: .trailing, spacing: 8) {
                if let nm = r.distanceToWaypointNM {
                    row("Next WPT", String(format: "%.2f nm", nm))
                }
                if let eta = r.etaSeconds {
                    row("ETA", Self.formatDuration(eta))
                }
                if let wps = r.waypoints, !wps.isEmpty {
                    row("Waypoints", "\(wps.count)")
                }
            }
            .panelStyle()
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        HStack(spacing: 16) {
            Text(label)
                .foregroundStyle(.secondary)
            Spacer(minLength: 0)
            Text(value)
                .font(.title3.monospacedDigit().bold())
        }
        .frame(minWidth: 300)
    }

    static func formatDuration(_ seconds: Double) -> String {
        let mins = Int((seconds / 60).rounded())
        if mins < 60 { return "\(mins) min" }
        return "\(mins / 60)h \(mins % 60)m"
    }
}

extension View {
    func panelStyle() -> some View {
        padding(24)
            .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 18))
            .foregroundStyle(.white)
    }
}
