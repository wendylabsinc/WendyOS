import Foundation

typealias VideoFrame = Wendy_Agent_Services_V1_VideoFrame
typealias VideoDevice = Wendy_Agent_Services_V1_VideoDevice

struct MonitorError: Error, CustomStringConvertible {
    let description: String
    init(_ description: String) { self.description = description }
}

struct ThermalReading: Sendable {
    let celsius: Double
    let x: Int
    let y: Int
    let provenance: String
    let summaryIdentity: [UInt16]?

    init(frame: VideoFrame, now: Date = Date()) throws {
        let age = now.timeIntervalSince1970 - Double(frame.timestampNs) / 1_000_000_000
        guard frame.timestampNs > 0, age >= -2, age <= 5 else {
            throw MonitorError(
                "No fresh thermal frame, timestamp is missing or more than 5 seconds old"
            )
        }
        let layout = frame.rawFormat
        let plane = layout.thermalPlane
        guard frame.codec == .raw, layout.hasThermalPlane, plane.version == 1 else {
            throw MonitorError("No supported thermal measurement plane")
        }
        if plane.adapter == "topdon-tc001-hik-v1" {
            // Same default-mode metadata decoder as Wendy's desktop viewer:
            // go/internal/agent/data/thermal/summary.go. This scale applies to
            // the summary temperature codes, NEVER to the raw sensor plane.
            guard layout.fourcc == "YUYV", layout.width == 256, layout.height == 392,
                layout.bytesPerLine == 512, frame.data.count == 200704
            else { throw MonitorError("Unsupported HIK temperature summary layout") }
            func word(_ offset: Int) -> UInt16 {
                UInt16(frame.data[offset]) | UInt16(frame.data[offset + 1]) << 8
            }
            let words = (0..<10).map { word(388 * 512 + $0 * 2) }
            guard word(193 * 512) == 0xccdd, word(193 * 512 + 2) == 0xaabb,
                words[0] == 0xffff, words[1] == 0xeeee,
                words[2] > 0, words[2] < 65535, words[5] > 0, words[5] < 65535,
                words[2] >= words[5],
                [3, 4, 6, 7].allSatisfy({ words[$0] <= 1000 })
            else { throw MonitorError("Invalid or uninitialized HIK temperature summary") }
            celsius = Double(words[2]) / 64 - 50
            x = (Int(words[3]) * 255 + 500) / 1000
            y = (Int(words[4]) * 191 + 500) / 1000
            provenance = "HIK camera-reported maximum, default-mode metadata, code / 64 - 50°C"
            summaryIdentity = words
            return
        }
        let conversion = plane.temperature
        guard plane.hasTemperature, conversion.kelvinPerCount.isFinite,
            conversion.kelvinPerCount > 0, conversion.kelvinOffset.isFinite,
            !conversion.provenance.isEmpty,
            (65535 * conversion.kelvinPerCount + conversion.kelvinOffset).isFinite
        else {
            throw MonitorError(
                "Camera provides raw counts without a verified temperature conversion; Celsius monitoring unavailable"
            )
        }
        let width = Int(plane.width)
        let height = Int(plane.height)
        let stride = Int(plane.rowStrideBytes)
        let offset = Int(plane.offsetBytes)
        guard width > 0, height > 0, width <= 4096, height <= 4096,
            width * height <= 4_000_000, stride >= width * 2, stride <= 65536,
            UInt64(layout.bytesPerLine) * UInt64(layout.height) == UInt64(frame.data.count),
            offset + (height - 1) * stride + width * 2 <= frame.data.count,
            plane.invalidValues.count <= 256, plane.invalidValues.allSatisfy({ $0 <= 65535 })
        else {
            throw MonitorError("Invalid thermal plane geometry or payload length")
        }
        let invalid = Set(plane.invalidValues)
        var hottest: (raw: UInt32, x: Int, y: Int)?
        frame.data.withUnsafeBytes { (bytes: UnsafeRawBufferPointer) in
            for y in 0..<height {
                for x in 0..<width {
                    let index = offset + y * stride + x * 2
                    let raw = UInt32(bytes[index]) | UInt32(bytes[index + 1]) << 8
                    if !invalid.contains(raw), hottest == nil || raw > hottest!.raw {
                        hottest = (raw, x, y)
                    }
                }
            }
        }
        guard let hottest else {
            throw MonitorError("Thermal frame contains no valid sensor samples")
        }
        celsius = Double(hottest.raw) * conversion.kelvinPerCount + conversion.kelvinOffset - 273.15
        x = hottest.x
        y = hottest.y
        provenance = conversion.provenance
        summaryIdentity = nil
    }
}

// The HIK summary updates around 2.5 Hz, independently of the 25 Hz video.
// Startup may contain cached metadata. Match the desktop viewer's freshness
// rule: wait for a change, and never refresh the age from identical summaries.
struct ThermalFrameDecoder {
    private var previousSummary: [UInt16]?
    private var summaryChangedAt: Date?

    mutating func decode(_ frame: VideoFrame, now: Date = Date()) throws -> ThermalReading {
        let reading = try ThermalReading(frame: frame, now: now)
        guard let identity = reading.summaryIdentity else { return reading }
        if let previousSummary, previousSummary != identity {
            summaryChangedAt = now
        }
        previousSummary = identity
        guard let summaryChangedAt else {
            throw MonitorError("Waiting for a fresh HIK temperature summary")
        }
        guard now.timeIntervalSince(summaryChangedAt) < 3 else {
            throw MonitorError(
                "HIK temperature summary has not changed for 3 seconds; waiting for a fresh measurement"
            )
        }
        return reading
    }
}
