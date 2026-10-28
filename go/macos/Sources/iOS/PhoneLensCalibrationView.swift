#if os(iOS)
import SwiftUI
import UIKit

struct PhoneLensCalibrationView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var feed: CameraFeed
    let completion: (SiteCameraLens) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var engine = ScreenLensCalibration()
    @State private var count = 0
    @State private var message = "Face this screen toward the ESP camera. Move and tilt it slowly."
    @State private var finished = false

    var body: some View {
        VStack(spacing: 12) {
            HStack {
                Button("Cancel") { dismiss() }
                Spacer()
                Text("\(count) / 12+ views").font(.headline).monospacedDigit()
            }.padding(.horizontal)
            Text(message).font(.callout).multilineTextAlignment(.center).frame(minHeight: 56).padding(.horizontal)
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
            Text("The phone vibrates when a new view is captured. Keep the camera fixed.")
                .font(.caption).multilineTextAlignment(.center).padding(.horizontal)
            // Keep a decoder display attached so existing stream ownership and
            // access-unit handling stay identical to the normal camera player.
            CameraDisplayView(decoder: feed.decoder).frame(width: 1, height: 1).opacity(0.001).accessibilityHidden(true)
        }
        .foregroundStyle(.black).background(.white).preferredColorScheme(.light)
        .padding(.vertical, 12)
        .interactiveDismissDisabled()
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
                guard model.deviceSession == session, feed.stream == stream, feed.running else { message = "The camera connection ended. Cancel and try again."; return }
                if let frame = feed.calibrationFrame(), frame.isFresh(at: Date()), frame.sequence != lastSequence {
                    lastSequence = frame.sequence
                    do {
                        let next = try await engine.add(frame)
                        try Task.checkCancellation()
                        if next > count {
                            count = next
                            UIImpactFeedbackGenerator(style: .light).impactOccurred()
                            if next >= 12 && next%3 == 0 {
                                do {
                                    let lens = try await engine.solve(width: frame.width, height: frame.height, codec: frame.codec)
                                    try Task.checkCancellation()
                                    finished = true
                                    UINotificationFeedbackGenerator().notificationOccurred(.success)
                                    completion(lens)
                                    dismiss()
                                    return
                                } catch is CancellationError { return }
                                catch { message = error.localizedDescription }
                            } else { message = "Captured. Tilt the screen and move it to another part of the camera view." }
                        }
                        if count >= 30 { message = "These views did not produce a stable lens calibration. Cancel and retry with more varied tilts."; return }
                    } catch is CancellationError { return }
                    catch { message = error.localizedDescription; return }
                }
                do { try await Task.sleep(for: .milliseconds(350)) } catch { return }
            }
        }
    }
}
#endif
