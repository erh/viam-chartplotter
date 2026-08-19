import Foundation

/// Polls the module's display API: /api/state every second, /api/route
/// every 5th tick, /api/info on connect (and occasionally, so a config
/// change on the machine shows up without restarting the app).
@MainActor
final class ChartplotterClient: ObservableObject {
    @Published private(set) var baseURL: URL?
    @Published private(set) var info: ServerInfo?
    @Published private(set) var state: BoatState?
    @Published private(set) var route: RouteInfo?
    @Published private(set) var lastError: String?

    private var pollTask: Task<Void, Never>?

    private static let savedURLKey = "chartplotterBaseURL"

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

    init() {
        if let s = UserDefaults.standard.string(forKey: Self.savedURLKey),
            let url = URL(string: s)
        {
            connect(to: url)
        }
    }

    func connect(to url: URL) {
        // Normalize to a trailing slash so path appends behave.
        var s = url.absoluteString
        if !s.hasSuffix("/") { s += "/" }
        guard let normalized = URL(string: s) else { return }
        baseURL = normalized
        UserDefaults.standard.set(normalized.absoluteString, forKey: Self.savedURLKey)
        startPolling()
    }

    func disconnect() {
        pollTask?.cancel()
        pollTask = nil
        baseURL = nil
        info = nil
        state = nil
        route = nil
        lastError = nil
        UserDefaults.standard.removeObject(forKey: Self.savedURLKey)
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
        } catch {
            lastError = error.localizedDescription
        }
        if tick % 5 == 0 {
            route = try? await get("api/route")
        }
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        guard let base = baseURL else { throw URLError(.badURL) }
        let (data, resp) = try await session.data(from: base.appendingPathComponent(path))
        guard let http = resp as? HTTPURLResponse, http.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }
        return try decoder.decode(T.self, from: data)
    }
}
