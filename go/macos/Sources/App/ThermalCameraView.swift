import SwiftUI
import CoreGraphics

/// Scale is an explicit operator choice, never inferred from Y16 or brightness.
enum ThermalCameraScale: String, CaseIterable, Identifiable {
    case counts = "Sensor counts"
    case kelvin100 = "TLinear 0.01 K"
    case kelvin10 = "TLinear 0.1 K"
    var id: String { rawValue }
    func label(_ raw: UInt16) -> String {
        switch self {
        case .counts: return "\(raw) counts"
        case .kelvin100: return String(format: "%.1f°C", Double(raw) / 100 - 273.15)
        case .kelvin10: return String(format: "%.1f°C", Double(raw) / 10 - 273.15)
        }
    }
}

struct ThermalCameraFrame {
    struct Point {
        let x: Int
        let y: Int
        let raw: UInt16
    }
    let image: CGImage
    let coldest: Point
    let hottest: Point
    let timestampNanoseconds: UInt64
    let kelvinPerCount: Double?
    let kelvinOffset: Double
    let allowScaleOverride: Bool

    func label(_ raw: UInt16, scale: ThermalCameraScale) -> String {
        if let kelvinPerCount {
            return String(format: "%.1f°C", Double(raw) * kelvinPerCount + kelvinOffset - 273.15)
        }
        return (allowScaleOverride ? scale : .counts).label(raw)
    }
    var width: Int { image.width }
    var height: Int { image.height }

    init(_ frame: WDThermalFrame, bytes: UnsafePointer<UInt8>?, count: Int) throws {
        let width = Int(frame.width), height = Int(frame.height)
        guard width > 0, height > 0, width <= 4096, height <= 4096, width * height <= 4_000_000,
              frame.has_temperature == 0 || (frame.kelvin_per_count.isFinite && frame.kelvin_per_count > 0 && frame.kelvin_offset.isFinite &&
                  (65535 * frame.kelvin_per_count + frame.kelvin_offset).isFinite),
              count == width * height * 4, let bytes,
              frame.cold_x < frame.width, frame.hot_x < frame.width,
              frame.cold_y < frame.height, frame.hot_y < frame.height,
              frame.cold_raw <= frame.hot_raw,
              let provider = CGDataProvider(data: Data(bytes: bytes, count: count) as CFData),
              let image = CGImage(width: width, height: height, bitsPerComponent: 8, bitsPerPixel: 32,
                                  bytesPerRow: width * 4, space: CGColorSpaceCreateDeviceRGB(),
                                  bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.last.rawValue),
                                  provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent) else {
            throw WendyError(message: "The camera sent an invalid thermal frame.")
        }
        self.image = image
        coldest = Point(x: Int(frame.cold_x), y: Int(frame.cold_y), raw: frame.cold_raw)
        hottest = Point(x: Int(frame.hot_x), y: Int(frame.hot_y), raw: frame.hot_raw)
        timestampNanoseconds = frame.timestamp_ns
        kelvinPerCount = frame.has_temperature != 0 ? frame.kelvin_per_count : nil
        kelvinOffset = frame.kelvin_offset
        allowScaleOverride = frame.allow_scale_override != 0
    }

    /// Pixel centers within an aspect-fit image, including letterboxing.
    static func position(x: Int, y: Int, width: Int, height: Int, in size: CGSize) -> CGPoint {
        let scale = min(size.width / CGFloat(width), size.height / CGFloat(height))
        return CGPoint(x: (size.width - CGFloat(width) * scale) / 2 + (CGFloat(x) + 0.5) * scale,
                       y: (size.height - CGFloat(height) * scale) / 2 + (CGFloat(y) + 0.5) * scale)
    }
}

struct ThermalCameraDisplay: View {
    @ObservedObject var feed: CameraFeed
    let frame: ThermalCameraFrame
    @State private var choosingScale = false

    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { context in
            let fresh = feed.running && context.date.timeIntervalSince(feed.thermalReceivedAt) < 3
            VStack(spacing: 0) {
                VStack(alignment: .leading, spacing: 5) {
                    HStack(spacing: 8) {
                        Image(systemName: "thermometer.medium")
                        Text(fresh ? "Thermal" : "Thermal · No recent frame")
                        Spacer(minLength: 0)
                        if frame.allowScaleOverride {
                            Button { choosingScale = true } label: { Image(systemName: "slider.horizontal.3") }
                                .buttonStyle(.plain).accessibilityLabel("Thermal temperature scale")
                        }
                    }.font(.caption.weight(.semibold))
                    if frame.kelvinPerCount == nil && !frame.allowScaleOverride {
                        Text("Sensor counts · Temperature calibration unavailable")
                    }
                    if fresh {
                        Text("+ Hottest \(frame.label(frame.hottest.raw, scale: feed.thermalScale))")
                            .foregroundStyle(.orange)
                        Text("− Coldest \(frame.label(frame.coldest.raw, scale: feed.thermalScale))")
                            .foregroundStyle(.cyan)
                    }
                }
                .font(.caption.monospacedDigit()).foregroundStyle(.white)
                .padding(10).background(.black.opacity(0.75), in: RoundedRectangle(cornerRadius: 8))
                .padding(10)
                ZStack {
                    Image(decorative: frame.image, scale: 1).resizable().interpolation(.none).scaledToFit()
                        .opacity(fresh ? 1 : 0.35)
                    if fresh {
                        GeometryReader { geometry in
                            marker(frame.coldest, label: "Coldest", symbol: "minus", color: .cyan, size: geometry.size)
                            marker(frame.hottest, label: "Hottest", symbol: "plus", color: .orange, size: geometry.size)
                        }.allowsHitTesting(false)
                    }
                }.padding(12)
            }
        }
        .popover(isPresented: $choosingScale) {
            VStack(alignment: .leading, spacing: 12) {
                Text("Thermal temperature scale").font(.headline)
                Picker("Sensor output", selection: $feed.thermalScale) {
                    ForEach(ThermalCameraScale.allCases) { Text($0.rawValue).tag($0) }
                }.pickerStyle(.menu)
                Text("Use a TLinear scale only when the camera is configured for radiometric output at that resolution. Y16 can also contain uncalibrated sensor counts.")
                    .font(.caption).foregroundStyle(.secondary)
                Text("The heatmap scales to each frame's measured range. Gray pixels have no valid measurement.")
                    .font(.caption).foregroundStyle(.secondary)
                Button("Done") { choosingScale = false }
            }.padding().frame(width: 300)
            .presentationCompactAdaptation(.popover)
        }
    }

    private func marker(_ point: ThermalCameraFrame.Point, label: String, symbol: String, color: Color, size: CGSize) -> some View {
        Image(systemName: symbol)
            .font(.system(size: 13, weight: .bold)).foregroundStyle(color)
            .frame(width: 20, height: 20).background(.black.opacity(0.65), in: Circle())
            .overlay(Circle().stroke(color, lineWidth: 1.5))
            .position(ThermalCameraFrame.position(x: point.x, y: point.y, width: frame.width, height: frame.height, in: size))
            .accessibilityLabel("\(label) \(frame.label(point.raw, scale: feed.thermalScale)), pixel \(point.x), \(point.y)")
    }
}
