import Foundation
import Network
import os

struct DiscoveredServer: Identifiable, Hashable {
    let name: String
    let endpoint: NWEndpoint
    var id: String { name }
}

/// Browses Bonjour for the Go module's `_viam-chartplotter._tcp`
/// advertisement, and resolves a picked service to a concrete
/// http://host:port/ URL by opening a TCP connection and reading the
/// remote endpoint (Bonjour service endpoints don't expose host/port
/// directly).
@MainActor
final class ServerDiscovery: ObservableObject {
    @Published private(set) var servers: [DiscoveredServer] = []
    private var browser: NWBrowser?

    func start() {
        stop()
        let b = NWBrowser(
            for: .bonjour(type: "_viam-chartplotter._tcp", domain: nil), using: .tcp)
        b.browseResultsChangedHandler = { [weak self] results, _ in
            let found = results.compactMap { r -> DiscoveredServer? in
                guard case let .service(name, _, _, _) = r.endpoint else { return nil }
                return DiscoveredServer(name: name, endpoint: r.endpoint)
            }.sorted { $0.name < $1.name }
            Task { @MainActor in self?.servers = found }
        }
        b.start(queue: .main)
        browser = b
    }

    func stop() {
        browser?.cancel()
        browser = nil
    }

    /// Prefer IPv4: the resolved address gets baked into a URL, and an
    /// IPv6 link-local address can't survive that — the interface zone
    /// ("%en0") isn't valid in a URL, and without it fe80:: is
    /// unroutable. A Mac advertises several link-local AAAAs, and a
    /// connection that happened to pick one produced a URL whose every
    /// poll then failed. Fall back to any-family only if v4 gets nowhere.
    nonisolated func resolveURL(for server: DiscoveredServer) async -> URL? {
        if let url = await resolveURL(for: server, ipv4Only: true) { return url }
        return await resolveURL(for: server, ipv4Only: false)
    }

    private nonisolated func resolveURL(for server: DiscoveredServer, ipv4Only: Bool) async -> URL? {
        await withCheckedContinuation { cont in
            let params = NWParameters.tcp
            if ipv4Only,
                let ip = params.defaultProtocolStack.internetProtocol as? NWProtocolIP.Options
            {
                ip.version = .v4
            }
            let conn = NWConnection(to: server.endpoint, using: params)
            // stateUpdateHandler can fire more than once; resume exactly
            // once. Lock-guarded so the capture is Swift 6-sendable.
            let finished = OSAllocatedUnfairLock(initialState: false)
            func finishOnce() -> Bool {
                finished.withLock { done in
                    if done { return false }
                    done = true
                    return true
                }
            }
            conn.stateUpdateHandler = { st in
                switch st {
                case .ready:
                    guard finishOnce() else { return }
                    var url: URL?
                    if let path = conn.currentPath,
                        case let .hostPort(host, port) = path.remoteEndpoint
                    {
                        url = URL(string: "http://\(Self.hostString(host)):\(port.rawValue)/")
                    }
                    conn.cancel()
                    cont.resume(returning: url)
                case .failed, .cancelled:
                    if finishOnce() {
                        cont.resume(returning: nil)
                    }
                default:
                    break
                }
            }
            conn.start(queue: .main)
        }
    }

    private nonisolated static func hostString(_ host: NWEndpoint.Host) -> String {
        // Network.framework renders scoped addresses as "addr%interface"
        // (IPv4 included, e.g. "192.168.25.31%en0"); the zone isn't valid
        // in a URL host, so strip it everywhere.
        func dropZone(_ s: String) -> String {
            s.split(separator: "%").first.map(String.init) ?? s
        }
        switch host {
        case .name(let n, _):
            return dropZone(n)
        case .ipv4(let a):
            return dropZone("\(a)")
        case .ipv6(let a):
            return "[\(dropZone("\(a)"))]"
        @unknown default:
            return dropZone("\(host)")
        }
    }
}
