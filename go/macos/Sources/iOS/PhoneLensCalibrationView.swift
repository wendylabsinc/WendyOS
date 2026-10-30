#if os(iOS)
import SwiftUI
import UIKit

struct PhoneLensCalibrationView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var feed: CameraFeed
    let completion: (SiteCameraLens) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var engine = ScreenLensCalibration()
    @State private var progress = ScreenLensProgress()
    @State private var lastSolvedRevision: UInt64 = 0
    @State private var failure: String?
    @State private var validating = false
    @State private var needsMoreAngles = false
    @State private var lastValidationFailure: String?
    @State private var showingHelp = false
    @State private var finished = false

    var body: some View {
        VStack(spacing: 12) {
            HStack {
                Button("Cancel") { dismiss() }
                Spacer()
                Text(validating ? "Checking lens…" : "\(progress.count) views in use").font(.headline).monospacedDigit()
                Spacer()
                Button("Help", systemImage: "questionmark.circle") { showingHelp = true }.labelStyle(.iconOnly)
            }.padding(.horizontal)
            HStack(spacing: 24) {
                VStack(spacing: 2) {
                    cameraProgress
                    Text("Camera view").font(.caption)
                }
                VStack(spacing: 6) {
                    PhoneCalibrationMotion(motion: progress.phoneMotion, paused: validating || failure != nil || showingHelp)
                        .frame(width: 100, height: 88)
                    Text("Move the phone\nKeep the camera still")
                        .font(.caption).multilineTextAlignment(.center)
                }
            }
            Text(failure ?? (validating ? "Hold still while the lens measurement is checked." : progress.phoneMotion.instruction))
                .font(.callout.weight(.medium)).multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true).padding(.horizontal)
            GeometryReader { geometry in
                let side = floor(min(geometry.size.width/9, geometry.size.height/12))
                Canvas { context, size in
                    // White border plus 7×10 exact square cells gives 6×9 inner
                    // corners. No physical screen size is needed for intrinsics.
                    let origin = CGPoint(x: (size.width-9*side)/2+side, y: (size.height-12*side)/2+side)
                    for row in 0..<10 {
                        for column in 0..<7 where (row+column)%2 == 0 {
                            let rect = CGRect(x: origin.x+CGFloat(column)*side, y: origin.y+CGFloat(row)*side, width: side, height: side)
                            context.fill(Path(rect), with: .color(.black))
                        }
                    }
                }
            }
            HStack(spacing: 12) {
                check("12+ views", complete: progress.count >= 12)
                check("Coverage", complete: progress.coverageReady)
                check("Lens checked", complete: finished)
            }
            Text(needsMoreAngles ? "Positions captured. More views at different angles are still needed." : "Green arcs show recorded positions. Pause until the phone vibrates.")
                .font(.caption).multilineTextAlignment(.center).padding(.horizontal)
        }
        .foregroundStyle(.black).background(.white).preferredColorScheme(.light)
        .padding(.vertical, 12)
        .interactiveDismissDisabled()
        .sheet(isPresented: $showingHelp) { help }
        .task {
            var ownedStream: UInt64 = 0
            let priorBrightness = UIScreen.main.brightness
            let priorIdle = UIApplication.shared.isIdleTimerDisabled
            UIScreen.main.brightness = max(priorBrightness, 0.85)
            UIApplication.shared.isIdleTimerDisabled = true
            defer {
                UIScreen.main.brightness = priorBrightness
                UIApplication.shared.isIdleTimerDisabled = priorIdle
                if ownedStream != 0, feed.stream == ownedStream {
                    feed.decoder.calibrationFrames.setEnabled(false)
                    model.pauseCamera(feed)
                }
            }
            feed.decoder.calibrationFrames.setEnabled(true)
            ownedStream = await model.playOwnedCamera(feed, inline: true)
            let session = model.deviceSession, stream = feed.stream
            var lastSequence: UInt64?
            while !Task.isCancelled && !finished {
                guard model.deviceSession == session, feed.stream == stream, feed.running else { failure = "The camera connection ended. Cancel and try again."; return }
                if showingHelp {
                    do { try await Task.sleep(for: .milliseconds(350)) } catch { return }
                    continue
                }
                if let frame = feed.calibrationFrame(), frame.isFresh(at: Date()), frame.sequence != lastSequence {
                    lastSequence = frame.sequence
                    do {
                        _ = try await engine.add(frame)
                        let next = try await engine.progress()
                        try Task.checkCancellation()
                        let captured = next.revision != progress.revision
                        progress = next
                        if !next.coverageReady { needsMoreAngles = false }
                        if captured {
                            UIImpactFeedbackGenerator(style: .light).impactOccurred()
                            if next.shouldSolve(after: lastSolvedRevision) {
                                lastSolvedRevision = next.revision
                                validating = true
                                do {
                                    let lens = try await engine.solve(width: frame.width, height: frame.height, codec: frame.codec)
                                    try Task.checkCancellation()
                                    finished = true
                                    UINotificationFeedbackGenerator().notificationOccurred(.success)
                                    completion(lens)
                                    dismiss()
                                    return
                                } catch is CancellationError { return }
                                catch { needsMoreAngles = true; lastValidationFailure = error.localizedDescription }
                                validating = false
                            }
                        }
                    } catch is CancellationError { return }
                    catch { failure = error.localizedDescription; return }
                }
                do { try await Task.sleep(for: .milliseconds(350)) } catch { return }
            }
        }
    }

    private func check(_ title: String, complete: Bool) -> some View {
        Label(title, systemImage: complete ? "checkmark.circle.fill" : "circle")
            .font(.caption).foregroundStyle(complete ? Color.green : .secondary)
            .accessibilityLabel("\(title): \(complete ? "complete" : "still needed")")
    }

    private var cameraProgress: some View {
        ZStack {
            // A single visible decoder display preserves stream ownership and
            // gives movement directions a concrete camera-space reference.
            ZStack {
                CameraDisplayView(decoder: feed.decoder)
                if let jpeg = feed.jpeg {
                    Image(decorative: jpeg, scale: 1).resizable().scaledToFit()
                }
            }.frame(width: 80, height: 64).accessibilityLabel("Live camera preview")
            ForEach(ScreenLensSide.allCases, id: \.self) { side in
                let recorded = progress.hasSample(toward: side)
                let active = progress.phoneMotion == .position(side)
                let angle = ringAngle(side)
                Circle().trim(from: 0.02, to: 0.23)
                    .stroke(recorded ? Color.green : active ? Color.blue : Color.gray.opacity(0.25), style: StrokeStyle(lineWidth: 5, lineCap: .round))
                    .rotationEffect(.degrees(angle - 45))
                    .frame(width: 112, height: 112)
                Image(systemName: recorded ? "checkmark.circle.fill" : "arrow.up.circle.fill")
                    .rotationEffect(.degrees(recorded ? 0 : angle + 90))
                    .foregroundStyle(recorded ? Color.green : active ? Color.blue : Color.gray)
                    .background(Circle().fill(.white))
                    .offset(x: 56 * cos(angle * .pi / 180), y: 56 * sin(angle * .pi / 180))
                    .accessibilityLabel("\(side.rawValue): \(recorded ? "position recorded" : "position still needed")")
            }
        }.frame(width: 132, height: 124)
    }

    private func ringAngle(_ side: ScreenLensSide) -> Double {
        switch side {
        case .right: return 0
        case .bottom: return 90
        case .left: return 180
        case .top: return 270
        }
    }

    private var help: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 24) {
                    Text("Keep the camera fixed and face this screen toward it. Move the phone as shown, then pause until it vibrates.")
                    ForEach([ScreenLensMotion.sideEdges, .topBottomEdges, .closer], id: \.instruction) { motion in
                        HStack(spacing: 20) {
                            PhoneCalibrationMotion(motion: motion).frame(width: 100, height: 100)
                            Text(motion.instruction)
                        }
                    }
                    Text("The ring shows positions recorded in the camera image. Blue points to the next position; green with a check means that position has a sample. Keep the entire white border visible.")
                    Text("The three checks track the number of views, image coverage, and the lens measurement. Green arcs alone do not mean calibration is complete. Angles still need to pass the lens check.")
                    Text("Capture pauses while this help is open. After 30 views, new captures replace older ones. Keep following the guide; you do not need to start over.")
                    if let lastValidationFailure {
                        Text("Last lens check: \(lastValidationFailure)").font(.callout)
                    }
                }.padding()
            }
            .navigationTitle("How to move the phone")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { showingHelp = false } } }
        }
    }
}

private struct PhoneCalibrationMotion: View {
    let motion: ScreenLensMotion
    var paused = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        TimelineView(.animation(minimumInterval: 1.0 / 30, paused: reduceMotion || paused)) { context in
            // Hold each end position so the demonstration includes the pause
            // required for a sharp capture, rather than continuous waving.
            let phase = context.date.timeIntervalSinceReferenceDate.truncatingRemainder(dividingBy: 8) / 8
            let wave = paused ? 0 : reduceMotion ? 0.7 : max(-1.0, min(1.0, sin(phase * 2 * .pi) * 1.8))
            Image(systemName: "iphone.gen3")
                .font(.system(size: 58, weight: .light))
                .foregroundStyle(.black)
                .rotation3DEffect(.degrees(motion == .sideEdges ? wave * 35 : 0), axis: (x: 0, y: 1, z: 0), perspective: 0.5)
                .rotation3DEffect(.degrees(motion == .topBottomEdges ? wave * 35 : 0), axis: (x: 1, y: 0, z: 0), perspective: 0.5)
                .scaleEffect(motion == .closer ? 1 + wave * 0.18 : 1)
                .offset(offset(wave))
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .accessibilityLabel(motion.instruction)
        .accessibilityHidden(true)
    }

    private func offset(_ wave: Double) -> CGSize {
        let distance = (wave + 1) * 10
        switch motion {
        case .position(.left): return CGSize(width: -distance, height: 0)
        case .position(.right): return CGSize(width: distance, height: 0)
        case .position(.top): return CGSize(width: 0, height: -distance)
        case .position(.bottom): return CGSize(width: 0, height: distance)
        default: return .zero
        }
    }
}
#endif
