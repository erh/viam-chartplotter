import Foundation
import Network

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

    nonisolated func resolveURL(for server: DiscoveredServer) async -> URL? {
        await withCheckedContinuation { cont in
            let conn = NWConnection(to: server.endpoint, using: .tcp)
            // stateUpdateHandler can fire more than once; resume exactly once.
            var finished = false
            conn.stateUpdateHandler = { st in
                switch st {
                case .ready:
                    guard !finished else { return }
                    finished = true
                    var url: URL?
                    if let path = conn.currentPath,
                        case let .hostPort(host, port) = path.remoteEndpoint
                    {
                        url = URL(string: "http://\(Self.hostString(host)):\(port.rawValue)/")
                    }
                    conn.cancel()
                    cont.resume(returning: url)
                case .failed, .cancelled:
                    if !finished {
                        finished = true
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
