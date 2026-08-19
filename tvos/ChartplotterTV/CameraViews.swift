import SwiftUI

/// Polls a display-API camera endpoint (/api/camera/{name}.jpg) and
/// shows the latest frame — the same still-frame model the web app
/// uses, no video stack. Thumbnails poll slowly; the full-screen view
/// polls faster.
struct PolledCameraImage: View {
    let url: URL
    var interval: TimeInterval = 2

    @State private var image: UIImage?

    var body: some View {
        ZStack {
            Color.black
            if let image {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFill()
            } else {
                ProgressView()
            }
        }
        .task(id: url) {
            while !Task.isCancelled {
                if let (data, resp) = try? await URLSession.shared.data(from: url),
                    (resp as? HTTPURLResponse)?.statusCode == 200,
                    let img = UIImage(data: data)
                {
                    image = img
                }
                try? await Task.sleep(nanoseconds: UInt64(interval * 1_000_000_000))
            }
        }
    }
}

struct CameraID: Identifiable {
    let name: String
    var id: String { name }
}

/// Bottom-of-chart thumbnail row; click a camera to go full screen.
struct CameraRow: View {
    @EnvironmentObject var client: ChartplotterClient
    @Binding var fullScreen: CameraID?

    var body: some View {
        HStack(spacing: 24) {
            ForEach(client.info?.cameras ?? [], id: \.self) { name in
                if let url = client.cameraURL(name) {
                    Button {
                        fullScreen = CameraID(name: name)
                    } label: {
                        PolledCameraImage(url: url)
                            .frame(width: 320, height: 180)
                            .clipShape(RoundedRectangle(cornerRadius: 12))
                            .overlay(alignment: .bottomLeading) {
                                Text(name)
                                    .font(.caption.bold())
                                    .padding(.horizontal, 10)
                                    .padding(.vertical, 4)
                                    .background(.black.opacity(0.6), in: Capsule())
                                    .foregroundStyle(.white)
                                    .padding(8)
                            }
                    }
                    .buttonStyle(.card)
                }
            }
        }
    }
}

/// Full-screen camera; Menu / back returns to the chart.
struct FullScreenCameraView: View {
    let camera: CameraID
    @EnvironmentObject var client: ChartplotterClient
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        ZStack(alignment: .topLeading) {
            Color.black.ignoresSafeArea()
            if let url = client.cameraURL(camera.name) {
                PolledCameraImage(url: url, interval: 1)
                    .ignoresSafeArea()
            }
            Text(camera.name)
                .font(.title3.bold())
                .padding(.horizontal, 16)
                .padding(.vertical, 8)
                .background(.black.opacity(0.6), in: Capsule())
                .foregroundStyle(.white)
                .padding(40)
        }
        .focusable()
        .onExitCommand { dismiss() }
    }
}
