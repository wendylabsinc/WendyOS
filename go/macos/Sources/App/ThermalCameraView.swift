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
        var celsius: Double? = nil
    }
    let image: CGImage
    let coldest: Point
    let hottest: Point
    let timestampNanoseconds: UInt64
    let kelvinPerCount: Double?
    let kelvinOffset: Double
    let allowScaleOverride: Bool
    let summaryColdest: Point?
    let summaryHottest: Point?
    let summaryUpdatedAt: Date?

    func summaryIsFresh(at date: Date) -> Bool {
        guard let summaryUpdatedAt else { return false }
        let age = date.timeIntervalSince(summaryUpdatedAt)
        return age >= -1 && age < 3
    }

    func temperatureLabel(_ point: Point, scale: ThermalCameraScale) -> String? {
        if let celsius = point.celsius { return String(format: "%.1f°C", celsius) }
        return temperatureLabel(point.raw, scale: scale)
    }

    func temperatureLabel(_ raw: UInt16, scale: ThermalCameraScale) -> String? {
        if let kelvinPerCount {
            return String(format: "%.1f°C", Double(raw) * kelvinPerCount + kelvinOffset - 273.15)
        }
        guard allowScaleOverride, scale != .counts else { return nil }
        return scale.label(raw)
    }
    var width: Int { image.width }
    var height: Int { image.height }

    init(_ frame: WDThermalFrame, bytes: UnsafePointer<UInt8>?, count: Int) throws {
        let width = Int(frame.width), height = Int(frame.height)
        guard width > 0, height > 0, width <= 4096, height <= 4096, width * height <= 4_000_000,
              frame.has_temperature == 0 || (frame.kelvin_per_count.isFinite && frame.kelvin_per_count > 0 && frame.kelvin_offset.isFinite &&
                  (65535 * frame.kelvin_per_count + frame.kelvin_offset).isFinite),
              frame.summary_updated_ns == 0 || (frame.summary_cold_celsius.isFinite && frame.summary_hot_celsius.isFinite &&
                  frame.summary_cold_celsius <= frame.summary_hot_celsius &&
                  frame.summary_cold_x < frame.width && frame.summary_hot_x < frame.width &&
                  frame.summary_cold_y < frame.height && frame.summary_hot_y < frame.height),
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
        summaryUpdatedAt = frame.summary_updated_ns == 0 ? nil : Date(timeIntervalSince1970: Double(frame.summary_updated_ns) / 1_000_000_000)
        summaryColdest = frame.summary_updated_ns == 0 ? nil : Point(x: Int(frame.summary_cold_x), y: Int(frame.summary_cold_y), raw: 0, celsius: frame.summary_cold_celsius)
        summaryHottest = frame.summary_updated_ns == 0 ? nil : Point(x: Int(frame.summary_hot_x), y: Int(frame.summary_hot_y), raw: 0, celsius: frame.summary_hot_celsius)
    }

    /// Pixel centers within an aspect-fit image, including letterboxing.
    static func position(x: Int, y: Int, width: Int, height: Int, in size: CGSize) -> CGPoint {
        let scale = min(size.width / CGFloat(width), size.height / CGFloat(height))
        return CGPoint(x: (size.width - CGFloat(width) * scale) / 2 + (CGFloat(x) + 0.5) * scale,
                       y: (size.height - CGFloat(height) * scale) / 2 + (CGFloat(y) + 0.5) * scale)
    }

    static func markerLabelPosition(_ position: CGPoint, avoiding other: CGPoint, isMinimum: Bool, in size: CGSize) -> CGPoint {
        func preferred(_ point: CGPoint) -> CGPoint {
            CGPoint(x: min(max(point.x + (point.x < size.width / 2 ? 46 : -46), 48), max(48, size.width - 48)),
                    y: point.y > size.height / 2 ? max(36, point.y - 19) : min(size.height - 36, max(46, point.y + 19)))
        }
        var label = preferred(position)
        let otherLabel = preferred(other)
        if abs(label.x - otherLabel.x) < 100 && abs(label.y - otherLabel.y) < 28 {
            let top = max(36, min(min(label.y, otherLabel.y), size.height - 64))
            label.y = top + (isMinimum ? 0 : 28)
        }
        return label
    }
}

struct ThermalCameraDisplay: View {
    @ObservedObject var feed: CameraFeed
    let frame: ThermalCameraFrame
    @State private var choosingScale = false

    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { context in
            let fresh = feed.running && context.date.timeIntervalSince(feed.thermalReceivedAt) < 3
            let summaryFresh = fresh && frame.summaryIsFresh(at: context.date)
            let coldest = summaryFresh ? (frame.summaryColdest ?? frame.coldest) : frame.coldest
            let hottest = summaryFresh ? (frame.summaryHottest ?? frame.hottest) : frame.hottest
            GeometryReader { geometry in
                ZStack(alignment: .topLeading) {
                    Image(decorative: frame.image, scale: 1)
                        .resizable().interpolation(.none).scaledToFit()
                        .frame(width: geometry.size.width, height: geometry.size.height)
                        .opacity(fresh ? 1 : 0.35)
                    if fresh {
                        marker(coldest, other: hottest, label: "MIN", color: .cyan, size: geometry.size)
                        marker(hottest, other: coldest, label: "MAX", color: .orange, size: geometry.size)
                    }
                    HStack(spacing: 5) {
                        Image(systemName: "thermometer.medium")
                        Text(fresh ? "THERMAL" : "NO RECENT FRAME")
                        if fresh && !summaryFresh && frame.kelvinPerCount == nil && feed.thermalScale == .counts {
                            Text("· RAW")
                        }
                    }
                    .font(.system(size: 9, weight: .semibold, design: .rounded))
                    .tracking(0.7).foregroundStyle(.white.opacity(0.9))
                    .padding(.horizontal, 8).padding(.vertical, 5)
                    .background(.black.opacity(0.4), in: Capsule())
                    .padding(12)
                }
                .overlay(alignment: .topTrailing) {
                    Button { choosingScale = true } label: {
                        Image(systemName: frame.allowScaleOverride ? "slider.horizontal.3" : "info.circle")
                            .font(.system(size: 12)).foregroundStyle(.white.opacity(0.8))
                            .padding(7).background(.black.opacity(0.35), in: Circle())
                    }
                    .buttonStyle(.plain).padding(10)
                    .accessibilityLabel("Thermal measurement details")
                }
            }
        }
        .popover(isPresented: $choosingScale) {
            VStack(alignment: .leading, spacing: 12) {
                Text("Thermal measurements").font(.headline)
                if frame.allowScaleOverride {
                    Picker("Sensor output", selection: $feed.thermalScale) {
                        ForEach(ThermalCameraScale.allCases) { Text($0.rawValue).tag($0) }
                    }.pickerStyle(.menu)
                    Text("Use a TLinear scale only when the camera is configured for radiometric output at that resolution.")
                        .font(.caption).foregroundStyle(.secondary)
                } else {
                    Text("MIN and MAX identify the coldest and hottest points. Camera-reported temperatures update separately from the live image. When temperatures are unavailable, only MIN and MAX are shown.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Text("The heatmap scales to each frame's sensor range. Gray pixels have no valid measurement.")
                    .font(.caption).foregroundStyle(.secondary)
                Button("Done") { choosingScale = false }
            }.padding().frame(width: 300)
            .presentationCompactAdaptation(.popover)
        }
    }

    private func marker(_ point: ThermalCameraFrame.Point, other: ThermalCameraFrame.Point, label: String, color: Color, size: CGSize) -> some View {
        let position = ThermalCameraFrame.position(x: point.x, y: point.y, width: frame.width, height: frame.height, in: size)
        let otherPosition = ThermalCameraFrame.position(x: other.x, y: other.y, width: frame.width, height: frame.height, in: size)
        let temperature = frame.temperatureLabel(point, scale: feed.thermalScale)
        return ZStack(alignment: .topLeading) {
            Image(systemName: "plus")
                .font(.system(size: 17, weight: .light)).foregroundStyle(color)
                .shadow(color: .black, radius: 1)
                .position(position)
            HStack(spacing: 4) {
                Text(label).font(.system(size: 8, weight: .bold, design: .rounded)).foregroundStyle(color)
                if let temperature {
                    Text(temperature)
                        .font(.system(size: 11, weight: .medium, design: .rounded).monospacedDigit())
                        .foregroundStyle(.white)
                }
            }
            .padding(.horizontal, 6).padding(.vertical, 4)
            .background(.black.opacity(0.65), in: RoundedRectangle(cornerRadius: 5))
            .fixedSize()
            .position(ThermalCameraFrame.markerLabelPosition(position, avoiding: otherPosition, isMinimum: label == "MIN", in: size))
        }
        .allowsHitTesting(false)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(label), \(temperature ?? "temperature unavailable"), pixel \(point.x), \(point.y)")
    }
}
