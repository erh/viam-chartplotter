import MapKit
import SwiftUI

/// The hosted checkmate tile server — same merged ENC+OSM raster the
/// web UI uses, served from the fast public host rather than the boat's
/// own renderer. The boat server still provides /api and cameras.
let checkmateTileServer = URL(string: "https://nycmaps.checkmatemaps.com/")!

/// Checkmate chart tiles. Mirrors the web app's per-zoom style split:
/// `ecdis` for the overview band, `wms` from z12 up. Unlike the web app
/// we don't pass `navaids=0` / `skip=` at high zoom — the TV draws no
/// vector layers, so navaids and structures stay baked into the tiles.
final class ChartTileOverlay: MKTileOverlay {
    private let baseURL: URL

    init(baseURL: URL) {
        self.baseURL = baseURL
        super.init(urlTemplate: nil)
        minimumZ = 7
        maximumZ = 17
        tileSize = CGSize(width: 256, height: 256)
    }

    override func url(forTilePath path: MKTileOverlayPath) -> URL {
        let style = path.z >= 12 ? "wms" : "ecdis"
        // Built as a string: appendingPathComponent would percent-encode
        // the query's "?" into the path.
        return URL(string: "noaa-enc/tile/\(path.z)/\(path.x)/\(path.y).png?style=\(style)", relativeTo: baseURL)!
    }
}

final class BoatAnnotation: MKPointAnnotation {}

/// Web app's speed-adjusted zoom (marineMap.svelte): zoom 16 stopped,
/// backing out as speed rises. Clamped to the chart's useful band.
func autoZoomLevel(sogKn: Double?) -> Double {
    let s = max(0, (sogKn ?? 0).rounded(.down))
    var z = (16 - pow(s, 0.41)).rounded(.down)
    if z <= 0 { z = 1 }
    return min(max(z, 8), 16)
}

struct ChartMapView: UIViewRepresentable {
    let baseURL: URL
    let state: BoatState?
    let route: RouteInfo?

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> MKMapView {
        let map = MKMapView()
        map.delegate = context.coordinator
        map.pointOfInterestFilter = .excludingAll
        // Never let the camera back out below the chart's z7/z8 band —
        // an overview render server-side is ~10s of wasted work for a
        // scale we don't show.
        map.cameraZoomRange = MKMapView.CameraZoomRange(
            minCenterCoordinateDistance: 500,
            maxCenterCoordinateDistance: 4_000_000)
        map.addOverlay(ChartTileOverlay(baseURL: checkmateTileServer), level: .aboveLabels)
        return map
    }

    func updateUIView(_ map: MKMapView, context: Context) {
        let co = context.coordinator
        guard let state else { return }
        let center = CLLocationCoordinate2D(latitude: state.lat, longitude: state.lng)

        // Boat marker, rotated to heading (map stays north-up).
        if co.boat == nil {
            let b = BoatAnnotation()
            co.boat = b
            map.addAnnotation(b)
        }
        co.boat?.coordinate = center
        co.heading = state.headingDeg ?? state.cogDeg ?? 0
        if let view = co.boat.flatMap(map.view(for:)) {
            view.transform = CGAffineTransform(rotationAngle: co.heading * .pi / 180)
        }

        // Auto-follow + speed zoom. setRegion only when the zoom band
        // changes; per-tick follow is a plain setCenter. Restarting a
        // full region animation every poll keeps MapKit from ever
        // committing tiles — the map stays blank.
        let zoom = autoZoomLevel(sogKn: state.sogKn)
        let moved =
            abs(center.latitude - co.lastCenter.latitude) > 1e-6
            || abs(center.longitude - co.lastCenter.longitude) > 1e-6
        if zoom != co.lastZoom {
            co.lastZoom = zoom
            co.lastCenter = center
            map.setRegion(region(center: center, zoom: zoom, size: map.bounds.size), animated: false)
        } else if moved {
            co.lastCenter = center
            map.setCenter(center, animated: true)
        }

        updateRouteOverlay(map, co: co, boat: center)
    }

    /// Convert a web-map tile zoom to a coordinate region for this view
    /// size: resolution (m/px) at zoom z is 156543·cos(lat)/2^z.
    private func region(center: CLLocationCoordinate2D, zoom: Double, size: CGSize) -> MKCoordinateRegion {
        let heightPx = size.height > 0 ? size.height : 1080
        let widthPx = size.width > 0 ? size.width : 1920
        let latRad = center.latitude * .pi / 180
        let metersPerPx = 156543.03392 * cos(latRad) / pow(2, zoom)
        let latDelta = Double(heightPx) * metersPerPx / 111_320
        let lngDelta = Double(widthPx) * metersPerPx / (111_320 * cos(latRad))
        return MKCoordinateRegion(
            center: center,
            span: MKCoordinateSpan(latitudeDelta: latDelta, longitudeDelta: lngDelta))
    }

    /// Magenta route line: boat → active-route waypoints (nav service),
    /// or boat → the nav system's destination when that's all we have.
    private func updateRouteOverlay(_ map: MKMapView, co: Coordinator, boat: CLLocationCoordinate2D) {
        var coords: [CLLocationCoordinate2D] = []
        if let wps = route?.waypoints, !wps.isEmpty {
            coords = [boat] + wps.map { CLLocationCoordinate2D(latitude: $0.lat, longitude: $0.lng) }
        } else if let dLat = route?.destinationLat, let dLng = route?.destinationLng {
            coords = [boat, CLLocationCoordinate2D(latitude: dLat, longitude: dLng)]
        }
        if let old = co.routeLine {
            map.removeOverlay(old)
            co.routeLine = nil
        }
        guard coords.count >= 2 else { return }
        let line = MKPolyline(coordinates: coords, count: coords.count)
        co.routeLine = line
        map.addOverlay(line, level: .aboveLabels)
    }

    final class Coordinator: NSObject, MKMapViewDelegate {
        var boat: BoatAnnotation?
        var routeLine: MKPolyline?
        var heading: Double = 0
        var lastZoom: Double = -1
        var lastCenter = CLLocationCoordinate2D(latitude: 0, longitude: 0)

        func mapView(_ mapView: MKMapView, rendererFor overlay: MKOverlay) -> MKOverlayRenderer {
            if let tiles = overlay as? MKTileOverlay {
                return MKTileOverlayRenderer(tileOverlay: tiles)
            }
            if let line = overlay as? MKPolyline {
                let r = MKPolylineRenderer(polyline: line)
                r.strokeColor = UIColor.magenta.withAlphaComponent(0.8)
                r.lineWidth = 4
                r.lineDashPattern = [10, 8]
                return r
            }
            return MKOverlayRenderer(overlay: overlay)
        }

        func mapView(_ mapView: MKMapView, viewFor annotation: MKAnnotation) -> MKAnnotationView? {
            guard annotation is BoatAnnotation else { return nil }
            let id = "boat"
            let view =
                mapView.dequeueReusableAnnotationView(withIdentifier: id)
                ?? MKAnnotationView(annotation: annotation, reuseIdentifier: id)
            view.annotation = annotation
            view.image = Self.boatImage
            view.transform = CGAffineTransform(rotationAngle: heading * .pi / 180)
            return view
        }

        /// A simple bow-up arrow: red fill, white outline, drawn once.
        static let boatImage: UIImage = {
            let size = CGSize(width: 44, height: 44)
            return UIGraphicsImageRenderer(size: size).image { ctx in
                let p = UIBezierPath()
                p.move(to: CGPoint(x: 22, y: 4))       // bow
                p.addLine(to: CGPoint(x: 34, y: 40))   // starboard quarter
                p.addLine(to: CGPoint(x: 22, y: 32))   // stern notch
                p.addLine(to: CGPoint(x: 10, y: 40))   // port quarter
                p.close()
                UIColor.systemRed.setFill()
                UIColor.white.setStroke()
                p.lineWidth = 2.5
                p.fill()
                p.stroke()
            }
        }()
    }
}
