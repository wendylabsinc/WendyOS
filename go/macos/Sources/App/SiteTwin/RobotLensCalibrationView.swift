import SwiftUI
import UniformTypeIdentifiers
import ImageIO

struct RobotCalibrationPattern: FileDocument {
    static var readableContentTypes: [UTType] { [.png] }
    var data: Data

    init() throws {
        guard let context = CGContext(data: nil, width: 900, height: 1200, bitsPerComponent: 8, bytesPerRow: 900,
                                      space: CGColorSpaceCreateDeviceGray(), bitmapInfo: CGImageAlphaInfo.none.rawValue) else { throw SiteLocalizationError.invalidImage }
        context.setFillColor(gray: 1, alpha: 1)
        context.fill(CGRect(x: 0, y: 0, width: 900, height: 1200))
        context.setFillColor(gray: 0, alpha: 1)
        for row in 0..<10 {
            for column in 0..<7 where (row + column) % 2 == 0 {
                context.fill(CGRect(x: (column + 1) * 100, y: (row + 1) * 100, width: 100, height: 100))
            }
        }
        let bytes = NSMutableData()
        guard let image = context.makeImage(), let output = CGImageDestinationCreateWithData(bytes, UTType.png.identifier as CFString, 1, nil) else { throw SiteLocalizationError.invalidImage }
        CGImageDestinationAddImage(output, image, nil)
        guard CGImageDestinationFinalize(output) else { throw SiteLocalizationError.invalidImage }
        data = bytes as Data
    }
    init(configuration: ReadConfiguration) throws { data = configuration.file.regularFileContents ?? Data() }
    func fileWrapper(configuration: WriteConfiguration) throws -> FileWrapper { FileWrapper(regularFileWithContents: data) }
}

private struct RobotCheckerboard: View {
    var body: some View {
        Canvas { context, size in
            context.fill(Path(CGRect(origin: .zero, size: size)), with: .color(.white))
            let side = floor(min(size.width / 9, size.height / 12))
            let origin = CGPoint(x: (size.width - side * 9) / 2 + side, y: (size.height - side * 12) / 2 + side)
            for row in 0..<10 {
                for column in 0..<7 where (row + column) % 2 == 0 {
                    let rect = CGRect(x: origin.x + CGFloat(column) * side, y: origin.y + CGFloat(row) * side, width: side, height: side)
                    context.fill(Path(rect), with: .color(.black))
                }
            }
        }.aspectRatio(3 / 4, contentMode: .fit)
            .accessibilityLabel("Calibration checkerboard: 7 columns and 10 rows, with a white border")
    }
}

struct RobotLensCalibrationView: View {
    @ObservedObject var model: SiteRobotMapModel
    @Environment(\.dismiss) private var dismiss
    @State private var session: RobotLensSession?
    @State private var engine = ScreenLensCalibration()
    @State private var samples = RobotLensSamples()
    @State private var count = 0
    @State private var capturing = false
    @State private var processing = false
    @State private var saved = false
    @State private var failed = false
    @State private var progress = ScreenLensProgress()
    @State private var validating = false
    @State private var lastSolvedRevision: UInt64 = 0
    @State private var message = "Keep Go2 stationary. Show the whole checkerboard to its front camera."
    @State private var pattern: RobotCalibrationPattern?
    @State private var exporting = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    Text("Go2 needs a lens measurement").font(.title2.bold())
                    Text("The wide-angle lens bends straight lines. Wendy needs to measure that distortion before it can compare this camera with your room scan. Capture starts automatically; keep Go2 stationary.")
                    Text("Move a flat checkerboard through the centre and edges of the camera view. Tilt it left/right and up/down, and vary its distance. Hold it still briefly at each position.")
                    #if os(macOS)
                    HStack(alignment: .top, spacing: 20) {
                        VStack(alignment: .leading, spacing: 10) {
                            preview
                            Text("Save the pattern, then display it on a phone or tablet, or print it on flat paper. Keep its white border visible and avoid stretching the image.")
                                .font(.callout).foregroundStyle(.secondary)
                        }.frame(maxWidth: .infinity)
                        RobotCheckerboard().frame(width: 210)
                    }
                    #else
                    RobotCheckerboard().frame(maxHeight: 460)
                    Text("Face this screen toward Go2, or use the saved pattern on another flat screen.")
                    preview.frame(maxHeight: 140)
                    #endif
                    HStack {
                        Text(saved ? "Calibration complete" : validating ? "Checking calibration…" : "\(count) views captured").font(.headline).monospacedDigit()
                        if processing { ProgressView().controlSize(.small) }
                    }
                    VStack(alignment: .leading, spacing: 6) {
                        check("At least 12 different views", complete: count >= 12)
                        check("Pattern covers enough of the camera image", complete: progress.coverageReady)
                        check("Lens measurement passes validation", complete: saved)
                    }
                    Text(saved || failed || validating || (lastSolvedRevision > 0 && progress.coverageReady && progress.detection >= 3) ? message : progress.nextStep)
                        .font(.headline).foregroundStyle(saved ? Color.green : .primary)
                        .fixedSize(horizontal: false, vertical: true)
                    if !saved { Text("Keep going until all three checks pass. The app will show Calibration complete and save the result automatically.").font(.callout).foregroundStyle(.secondary) }
                    if let mode = samples.mode { Text(mode.label).font(.caption).foregroundStyle(.secondary) }
                    HStack {
                        Button("Save checkerboard…", systemImage: "square.and.arrow.down") {
                            do { pattern = try RobotCalibrationPattern(); exporting = true }
                            catch { message = error.localizedDescription }
                        }.buttonStyle(.bordered)
                    }
                    Text("Only a measurement that passes checks on separate views is saved. The lens is retained for this robot and camera mode across room scans.")
                        .font(.caption).foregroundStyle(.secondary)
                }.padding(20)
            }
            .navigationTitle("Go2 Lens Calibration")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button(saved ? "Target removed — Continue" : "Cancel") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(width: 760, height: 680)
        #endif
        .interactiveDismissDisabled(processing)
        .fileExporter(isPresented: $exporting, document: pattern, contentType: .png, defaultFilename: "Go2-calibration-checkerboard") { result in
            if case .failure(let error) = result { message = error.localizedDescription }
        }
        .onDisappear { if let session { model.endLensCalibration(session, completed: saved) } }
        .task { await observe() }
    }

    private func check(_ title: String, complete: Bool) -> some View {
        Label(title, systemImage: complete ? "checkmark.circle.fill" : "circle")
            .foregroundStyle(complete ? Color.green : .secondary).font(.callout)
    }

    private var preview: some View {
        Group {
            if let decoder = model.cameraDecoder { CameraDisplayView(decoder: decoder) }
            else { Rectangle().fill(.black).overlay { ProgressView("Waiting for camera…") } }
        }.aspectRatio(16 / 9, contentMode: .fit).clipShape(RoundedRectangle(cornerRadius: 10))
    }

    private func observe() async {
        do {
            let token = try model.beginLensCalibration()
            session = token; capturing = true
            while !Task.isCancelled {
                guard let frame = try model.lensCalibrationFrame(token), frame.isFresh(at: .now) else {
                    message = "Waiting for a fresh Go2 camera frame."
                    try await Task.sleep(for: .milliseconds(350)); continue
                }
                if capturing && !failed && !saved {
                    processing = true
                    do {
                        if try samples.accept(frame, now: .now) {
                            _ = try await engine.add(frame)
                            progress = try await engine.progress()
                            try Task.checkCancellation()
                            count = progress.count
                            if count >= 12 && progress.coverageReady && progress.revision >= lastSolvedRevision + 3 {
                                lastSolvedRevision = progress.revision
                                validating = true
                                message = "Checking the lens against separate views. The live preview will keep updating."
                                do {
                                    let lens = try await engine.solve(width: frame.width, height: frame.height, codec: frame.codec)
                                    try Task.checkCancellation()
                                    try await model.saveMeasuredLens(lens, session: token, mode: RobotCameraMode(frame))
                                    saved = true; capturing = false
                                    message = "Calibration complete and saved. Remove the checkerboard, then choose Continue to find Go2's room position."
                                } catch is CancellationError { throw CancellationError() }
                                catch { message = error.localizedDescription }
                                validating = false
                            }
                        }
                    } catch is CancellationError { throw CancellationError() }
                    catch { message = error.localizedDescription; capturing = false; failed = true }
                    processing = false
                }
                try await Task.sleep(for: .milliseconds(350))
            }
        } catch is CancellationError { processing = false }
        catch { message = error.localizedDescription; processing = false; capturing = false; failed = true; if let session { model.endLensCalibration(session) }; session = nil }
    }
}
