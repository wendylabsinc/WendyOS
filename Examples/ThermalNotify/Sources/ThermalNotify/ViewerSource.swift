import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// Celsius frames from the MLX90642 USB viewer, not the unrelated UVC preview.
struct ViewerEnvelope: Decodable {
    struct Frame: Decodable {
        let width: Int
        let height: Int
        let pixels: [Double]
        let seq: UInt64
        let timestamp_us: UInt64
        let number: UInt64
        var identity: String { "\(number)/\(seq)/\(timestamp_us)" }
    }
    let status: String
    let age: Double?
    let frame: Frame?

    func validated() throws -> (Frame, ThermalReading) {
        guard status == "live", let age, age.isFinite, (0..<3).contains(age), let frame,
            frame.width == 32, frame.height == 24, frame.pixels.count == 768, frame.timestamp_us > 0,
            frame.pixels.allSatisfy({ $0.isFinite && (-273.15...1_000).contains($0) }),
            let hottest = frame.pixels.indices.max(by: { frame.pixels[$0] < frame.pixels[$1] })
        else { throw MonitorError("MLX90642 viewer has no valid fresh temperature frame") }
        return (frame, ThermalReading(celsius: frame.pixels[hottest], x: hottest % frame.width, y: hottest / frame.width,
            provenance: "MLX90642 Celsius samples via Wendy USB thermal viewer"))
    }
}

enum ViewerSource {
    static let camera: VideoDevice = .with {
        $0.name = "MLX90642 USB thermal sensor"
        $0.stableID = "app:dev.wendy.thermal-viewer/mlx90642"
    }

    static func read(_ url: URL) async throws -> (ViewerEnvelope.Frame, ThermalReading) {
        var request = URLRequest(url: url)
        request.timeoutInterval = 5
        request.cachePolicy = .reloadIgnoringLocalCacheData
        let (data, response) = try await URLSession.shared.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200, data.count <= 65_536 else {
            throw MonitorError("Thermal viewer returned an invalid response")
        }
        return try JSONDecoder().decode(ViewerEnvelope.self, from: data).validated()
    }

    static func monitor(_ url: URL, alerts: AlertCoordinator) async throws {
        var identity: String?
        var changedAt = Date.distantPast
        var lastProblem: String?
        var observations = 0
        while !Task.isCancelled {
            do {
                let (frame, reading) = try await read(url)
                if identity != frame.identity { identity = frame.identity; changedAt = .now; observations += 1 }
                guard Date().timeIntervalSince(changedAt) < 3 else { throw MonitorError("Thermal viewer is repeating an old frame") }
                if observations == 1 || observations % 60 == 0 || lastProblem != nil {
                    log("MLX90642 maximum \(String(format: "%.2f", reading.celsius))°C at (\(reading.x), \(reading.y)); frame \(frame.seq)")
                }
                lastProblem = nil
                await alerts.observe(camera: camera, reading: reading)
            } catch {
                if Task.isCancelled { return }
                let problem = String(describing: error)
                if problem != lastProblem { log(problem); lastProblem = problem }
            }
            try await Task.sleep(for: .milliseconds(500))
        }
    }
}
