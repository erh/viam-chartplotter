import SwiftUI

@main
struct ChartplotterTVApp: App {
    @StateObject private var client = ChartplotterClient()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(client)
        }
    }
}
