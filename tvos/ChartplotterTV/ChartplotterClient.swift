import Foundation

/// A non-200 answer from the server: the chartplotter is reachable, the
/// endpoint just can't serve right now (e.g. "no movement_sensor
/// configured"). Kept distinct from transport errors so the UI never calls
/// a healthy server "connection lost".
enum APIError: LocalizedError {
    case http(status: Int, message: String?)

    var errorDescription: String? {
        switch self {
        case let .http(status, message):
            return message ?? "server error \(status)"
        }
    }
}

private struct APIErrorBody: Decodable {
    let error: String
}

/// Polls the module's display API: /api/state every second, /api/route
/// every 5th tick, /api/info on connect (and occasionally, so a config
/// change on the machine shows up without restarting the app).
@MainActor
final class ChartplotterClient: ObservableObject {
    @Published private(set) var baseURL: URL?
    @Published private(set) var info: ServerInfo?
    @Published private(set) var state: BoatState?
    @Published private(set) var route: RouteInfo?
    @Published private(set) var track: [TrackPoint] = []
    @Published private(set) var lastError: String?
    /// Consecutive /api/state polls that failed at the transport level —
    /// the server didn't answer at all. The UI treats >= 3 (~3s) as
    /// "connection lost": banner up, panels greyed, chart kept as-is.
    /// An answered non-200 (e.g. 503 "no movement_sensor configured")
    /// does NOT count: the server is fine, that data just isn't there.
    @Published private(set) var consecutiveFailures = 0

    var isOffline: Bool { consecutiveFailures >= 3 }

    private var pollTask: Task<Void, Never>?

    private static let savedURLKey = "chartplotterBaseURL"
    private static let savedNameKey = "chartplotterServerName"

    /// The remembered server, for the discovery screen to reconnect to —
    /// but only once it's confirmed online (found via Bonjour by name, or
    /// probed by URL for manual entries), never blindly.
    var savedURL: URL? {
        UserDefaults.standard.string(forKey: Self.savedURLKey).flatMap { URL(string: $0) }
    }
    /// Bonjour service name of the remembered server; nil after a manual
    /// host:port connect (no name to watch for).
    var savedServerName: String? {
        UserDefaults.standard.string(forKey: Self.savedNameKey)
    }

    private let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        return d
    }()

    private let session: URLSession = {
        let c = URLSessionConfiguration.default
        c.timeoutIntervalForRequest = 5
        c.requestCachePolicy = .reloadIgnoringLocalCacheData
        return URLSession(configuration: c)
    }()

    // No auto-connect on launch: the discovery screen reconnects to the
    // remembered server once it's seen online, so a moved/offline boat
    // lands on the picker instead of an endless "connection lost".

    /// True after switchServer(): the user deliberately went back to the
    /// chooser, so it must sit still instead of auto-connecting them
    /// right back to the only boat around. Cleared by the next connect.
    private(set) var userIsChoosing = false

    /// Leave the current server to pick another: back to the chooser,
    /// with the remembered choice cleared and auto-connect suppressed.
    func switchServer() {
        userIsChoosing = true
        disconnect()
    }

    func connect(to url: URL, rememberName: String? = nil) {
        userIsChoosing = false
        // Normalize to a trailing slash so path appends behave.
        var s = url.absoluteString
        if !s.hasSuffix("/") { s += "/" }
        guard let normalized = URL(string: s) else { return }
        baseURL = normalized
        UserDefaults.standard.set(normalized.absoluteString, forKey: Self.savedURLKey)
        if let rememberName {
            UserDefaults.standard.set(rememberName, forKey: Self.savedNameKey)
        } else {
            UserDefaults.standard.removeObject(forKey: Self.savedNameKey)
        }
        startPolling()
    }

    /// One quick probe: is a chartplotter answering at this URL? Used for
    /// remembered manual entries, which have no Bonjour name to watch for.
    nonisolated static func isReachable(_ url: URL) async -> Bool {
        var req = URLRequest(url: url.appendingPathComponent("api/info"))
        req.timeoutInterval = 3
        guard let (_, resp) = try? await URLSession.shared.data(for: req),
            (resp as? HTTPURLResponse)?.statusCode == 200
        else { return false }
        return true
    }

    func disconnect() {
        pollTask?.cancel()
        pollTask = nil
        baseURL = nil
        info = nil
        state = nil
        route = nil
        track = []
        lastError = nil
        consecutiveFailures = 0
        UserDefaults.standard.removeObject(forKey: Self.savedURLKey)
        UserDefaults.standard.removeObject(forKey: Self.savedNameKey)
    }

    func cameraURL(_ name: String) -> URL? {
        baseURL?.appendingPathComponent("api/camera/\(name).jpg")
    }

    private func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            var tick = 0
            while !Task.isCancelled {
                await self?.pollOnce(tick: tick)
                tick += 1
                try? await Task.sleep(nanoseconds: 1_000_000_000)
            }
        }
    }

    private func pollOnce(tick: Int) async {
        guard baseURL != nil else { return }
        if info == nil || tick % 60 == 0 {
            info = try? await get("api/info")
        }
        do {
            state = try await get("api/state") as BoatState
            lastError = nil
            consecutiveFailures = 0
        } catch let error as APIError {
            // Server answered — connection is fine; show why there's no
            // state (the data bar renders lastError when state is nil)
            // instead of stale numbers or a "connection lost" banner.
            state = nil
            lastError = error.localizedDescription
            consecutiveFailures = 0
        } catch {
            lastError = error.localizedDescription
            consecutiveFailures += 1
        }
        if tick % 5 == 0 {
            route = try? await get("api/route")
        }
        if tick % 30 == 0 {
            // Older module deployments don't have /api/track; leave the
            // track empty rather than erroring.
            if let resp = try? await get("api/track") as TrackResponse {
                track = resp.points
            }
        }
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        guard let base = baseURL else { throw URLError(.badURL) }
        let (data, resp) = try await session.data(from: base.appendingPathComponent(path))
        guard let http = resp as? HTTPURLResponse else {
            throw URLError(.badServerResponse)
        }
        guard http.statusCode == 200 else {
            let message = (try? decoder.decode(APIErrorBody.self, from: data))?.error
            throw APIError.http(status: http.statusCode, message: message)
        }
        return try decoder.decode(T.self, from: data)
    }
}
