import ARKit
import AVFoundation
import SwiftUI
import Vision
import Accelerate

struct DeviceRecognitionFrame: Sendable {
    let gray: Data
    let width: Int
    let height: Int
    let display: CGAffineTransform
    let timestamp: TimeInterval
    let textOrientation: CGImagePropertyOrientation
    let textToImage: CGAffineTransform
}

private actor DeviceVisionProcessor {
    func recognizeLabels(_ frame: DeviceRecognitionFrame, devices: [Device]) throws -> [DeviceSight] {
        var result: [DeviceSight] = []
        guard !devices.isEmpty, let provider = CGDataProvider(data: frame.gray as CFData),
              let image = CGImage(width: frame.width, height: frame.height, bitsPerComponent: 8, bitsPerPixel: 8,
                bytesPerRow: frame.width, space: CGColorSpaceCreateDeviceGray(), bitmapInfo: CGBitmapInfo(rawValue: 0),
                provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent) else { return result }
        // OCR also works on an existing hostname label. No label is required for
        // feature recognition, and a matching label is not device authentication.
        let request = VNRecognizeTextRequest()
        request.recognitionLevel = .accurate
        request.usesLanguageCorrection = false
        try VNImageRequestHandler(cgImage: image, orientation: frame.textOrientation).perform([request])
        var labelOccurrences: [String: Int] = [:]
        for observation in request.results ?? [] {
            guard let text = observation.topCandidates(1).first, text.confidence >= 0.8 else { continue }
            let indices = DeviceLabelMatcher.matches(text.string, names: devices.map(\.title))
            guard !indices.isEmpty else { continue }
            let box = observation.boundingBox
            let corners = [CGPoint(x: box.minX, y: 1-box.maxY), CGPoint(x: box.maxX, y: 1-box.maxY),
                           CGPoint(x: box.maxX, y: 1-box.minY), CGPoint(x: box.minX, y: 1-box.minY)]
            let labelKey = indices.map { devices[$0].id }.sorted().joined(separator: "|")
            let occurrence = labelOccurrences[labelKey, default: 0]
            labelOccurrences[labelKey] = occurrence + 1
            result.append(DeviceSight(id: "label:\(labelKey):\(occurrence)",
                label: text.string, deviceIDs: indices.map { devices[$0].id },
                corners: corners.map { $0.applying(frame.textToImage).applying(frame.display) }, evidence: "Visible device name"))
        }
        return result
    }
}

@Observable @MainActor
final class DeviceViewfinderController {
    let session = ARSession()
    let engine = DeviceRecognitionEngine()
    private let processor = DeviceVisionProcessor()
    var allowed = false
    var error: String?
    var tracking = "Point at a device and move closer."
    private var appearances: [DeviceSight] = []
    private var labels: [DeviceSight] = []
    private var appearanceTime = Date.distantPast
    private var labelTime = Date.distantPast
    var sights: [DeviceSight] { visibleSights(at: Date()) }
    var diagnostics = "Waiting for the first image…"
    var savedDiagnostic = false
    var savingDiagnostic = false
    func visibleSights(at date: Date) -> [DeviceSight] {
        (date.timeIntervalSince(appearanceTime) < 1.5 ? appearances : [])
            + (date.timeIntervalSince(labelTime) < 1.5 ? labels : [])
    }
    var viewport = CGSize(width: 1, height: 1)
    var orientation: UIInterfaceOrientation = .portrait
    var frame: DeviceRecognitionFrame?
    var devices: [Device] = []

    func run(manageSession: Bool = true) async {
        guard ARWorldTrackingConfiguration.isSupported else {
            error = "Device recognition needs an iPhone or iPad with ARKit support."
            return
        }
        allowed = await AVCaptureDevice.requestAccess(for: .video)
        guard allowed, !Task.isCancelled else {
            if !allowed { error = "Allow camera access in Settings to recognize devices." }
            return
        }
        if manageSession {
            let config = ARWorldTrackingConfiguration()
            session.run(config, options: [.resetTracking, .removeExistingAnchors])
        }
        defer {
            if manageSession { session.pause() }
            appearances = []; labels = []; frame = nil
        }
        do {
            try await engine.load()
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask { try await self.recognizeAppearances() }
                group.addTask { try await self.recognizeLabels() }
                defer { group.cancelAll() }
                _ = try await group.next()
            }
        } catch is CancellationError {} catch { self.error = error.localizedDescription }
    }

    private func recognizeAppearances() async throws {
        var confirmation = DeviceSightConfirmation()
        var lastTimestamp: TimeInterval = -1
        while !Task.isCancelled {
            guard let arFrame = session.currentFrame, arFrame.timestamp > lastTimestamp else {
                appearances = []; frame = nil; confirmation.reset()
                tracking = "Waiting for the camera…"
                try await Task.sleep(for: .milliseconds(200)); continue
            }
            lastTimestamp = arFrame.timestamp
            let captured = Self.copy(arFrame, orientation: orientation, viewport: viewport)
            frame = captured
            let started = ContinuousClock.now
            let report = try await engine.inspect(gray: captured.gray, width: captured.width, height: captured.height)
            try Task.checkCancellation()
            let duration = started.duration(to: .now)
            let milliseconds = Double(duration.components.seconds)*1000 + Double(duration.components.attoseconds)/1e15
            diagnostics = report.summary + String(format: " · %.0f ms", milliseconds)
            let age = (session.currentFrame?.timestamp ?? captured.timestamp) - captured.timestamp
            if age < 1.5 {
                appearances = confirmation.accept(DeviceSight.group(report.matches, display: captured.display), timestamp: captured.timestamp)
                appearanceTime = Date()
                tracking = appearances.isEmpty ? report.guidance : "Tap a device card to open it."
            } else {
                appearances = []; confirmation.reset()
                tracking = "Recognition is running slowly. Keep the board centred and hold steady."
            }
            error = nil
            try await Task.sleep(for: .milliseconds(120))
        }
    }

    private func recognizeLabels() async throws {
        var confirmation = DeviceSightConfirmation()
        while !Task.isCancelled {
            if !devices.isEmpty, let arFrame = session.currentFrame {
                let captured = Self.copy(arFrame, orientation: orientation, viewport: viewport)
                do {
                    let results = try await processor.recognizeLabels(captured, devices: devices)
                    try Task.checkCancellation()
                    if (session.currentFrame?.timestamp ?? captured.timestamp) - captured.timestamp < 1.5 {
                        labels = confirmation.accept(results, timestamp: captured.timestamp)
                        labelTime = Date()
                    } else { labels = []; confirmation.reset() }
                } catch is CancellationError { throw CancellationError() }
                catch { labels = []; confirmation.reset() } // OCR failure must not suppress board recognition.
            } else { labels = []; confirmation.reset() }
            try await Task.sleep(for: .milliseconds(500))
        }
    }

    func saveDiagnosticFrame() async {
        guard let captured = frame, !savingDiagnostic else { return }
        savingDiagnostic = true
        defer { savingDiagnostic = false }
        do {
            let report = try await engine.inspect(gray: captured.gray, width: captured.width, height: captured.height)
            try await engine.saveDiagnostic(gray: captured.gray, width: captured.width, height: captured.height, report: report.summary)
            savedDiagnostic = true
        } catch { self.error = error.localizedDescription }
    }

    func reference(for device: Device, frame: DeviceRecognitionFrame) -> DeviceVisualReference {
        let inverse = frame.display.inverted()
        let corners = Self.reticle.map { $0.applying(inverse) }
        return DeviceVisualReference(label: device.title, deviceID: device.id, width: frame.width, height: frame.height,
            gray: frame.gray, quad: corners.flatMap { [Double($0.x) * Double(frame.width), Double($0.y) * Double(frame.height)] })
    }
    static let reticle = [CGPoint(x: 0.2, y: 0.3), CGPoint(x: 0.8, y: 0.3), CGPoint(x: 0.8, y: 0.7), CGPoint(x: 0.2, y: 0.7)]

    private static func copy(_ frame: ARFrame, orientation: UIInterfaceOrientation, viewport: CGSize) -> DeviceRecognitionFrame {
        let buffer = frame.capturedImage
        CVPixelBufferLockBaseAddress(buffer, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(buffer, .readOnly) }
        let sourceWidth = CVPixelBufferGetWidthOfPlane(buffer, 0)
        let sourceHeight = CVPixelBufferGetHeightOfPlane(buffer, 0)
        let scale = min(1, 1920 / Double(max(sourceWidth, sourceHeight)))
        let width = Int(Double(sourceWidth)*scale), height = Int(Double(sourceHeight)*scale)
        let stride = CVPixelBufferGetBytesPerRowOfPlane(buffer, 0)
        var gray = Data(count: width * height)
        if let source = CVPixelBufferGetBaseAddressOfPlane(buffer, 0) {
            gray.withUnsafeMutableBytes { raw in
                var input = vImage_Buffer(data: source, height: vImagePixelCount(sourceHeight), width: vImagePixelCount(sourceWidth), rowBytes: stride)
                var output = vImage_Buffer(data: raw.baseAddress, height: vImagePixelCount(height), width: vImagePixelCount(width), rowBytes: width)
                _ = vImageScale_Planar8(&input, &output, nil, vImage_Flags(kvImageHighQualityResampling))
            }
        }
        let textOrientation: CGImagePropertyOrientation
        let textToImage: CGAffineTransform
        switch orientation {
        case .portrait:
            textOrientation = .right
            textToImage = CGAffineTransform(a: 0, b: -1, c: 1, d: 0, tx: 0, ty: 1)
        case .portraitUpsideDown:
            textOrientation = .left
            textToImage = CGAffineTransform(a: 0, b: 1, c: -1, d: 0, tx: 1, ty: 0)
        case .landscapeLeft:
            textOrientation = .down
            textToImage = CGAffineTransform(a: -1, b: 0, c: 0, d: -1, tx: 1, ty: 1)
        default:
            textOrientation = .up
            textToImage = .identity
        }
        return DeviceRecognitionFrame(gray: gray, width: width, height: height,
            display: frame.displayTransform(for: orientation, viewportSize: viewport), timestamp: frame.timestamp,
            textOrientation: textOrientation, textToImage: textToImage)
    }
}

struct DeviceViewfinderSurface: UIViewRepresentable {
    let controller: DeviceViewfinderController
    func makeUIView(context: Context) -> ARSCNView {
        let view = ARSCNView(frame: .zero)
        view.session = controller.session
        view.automaticallyUpdatesLighting = false
        return view
    }
    func updateUIView(_ view: ARSCNView, context: Context) {
        if view.bounds.width > 1, view.bounds.height > 1 { controller.viewport = view.bounds.size }
        if let orientation = view.window?.windowScene?.effectiveGeometry.interfaceOrientation { controller.orientation = orientation }
    }
}
