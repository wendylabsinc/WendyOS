import SwiftUI
import UIKit
import AVFoundation
import SafariServices
#if targetEnvironment(simulator) && DEBUG
import UniformTypeIdentifiers
#endif

struct BrowserSheet: UIViewControllerRepresentable {
    let url: URL
    func makeUIViewController(context: Context) -> SFSafariViewController { SFSafariViewController(url: url) }
    func updateUIViewController(_ controller: SFSafariViewController, context: Context) {}
}

#if targetEnvironment(simulator) && DEBUG
struct SimulatorSignInLink: View {
    let url: URL
    @State private var copied = false

    var body: some View {
        VStack(spacing: 5) {
            Button(copied ? "Copy sign-in link again" : "Copy sign-in link for Mac") {
                UIPasteboard.general.setItems([[UTType.url.identifier: url, UTType.utf8PlainText.identifier: url.absoluteString]],
                                              options: [.localOnly: true, .expirationDate: Date().addingTimeInterval(300)])
                copied = true
            }
            Text(copied ? "Paste in Safari on your Mac. Keep Wendy open." : "Use your Mac or iPhone passkey in Safari on your Mac.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity).padding(10).background(.bar)
    }
}
#endif

private final class VideoHostView: UIView {
    let displayLayer = AVSampleBufferDisplayLayer()
    override init(frame: CGRect) {
        super.init(frame: frame)
        backgroundColor = .black
        layer.addSublayer(displayLayer)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
    override func layoutSubviews() {
        super.layoutSubviews()
        CATransaction.begin(); CATransaction.setDisableActions(true)
        displayLayer.frame = bounds
        CATransaction.commit()
    }
}
struct CameraDisplayView: UIViewRepresentable {
    let decoder: H264AnnexBDecoder
    func makeCoordinator() -> H264AnnexBDecoder { decoder }
    func makeUIView(context: Context) -> UIView {
        let view = VideoHostView()
        decoder.attach(layer: view.displayLayer)
        return view
    }
    func updateUIView(_ view: UIView, context: Context) {}
    static func dismantleUIView(_ view: UIView, coordinator: H264AnnexBDecoder) {
        if let view = view as? VideoHostView { coordinator.detach(layer: view.displayLayer) }
    }
}

// Compact sheets stack their panes; iPad retains side-by-side content.
struct HSplitView<Content: View>: View {
    @Environment(\.horizontalSizeClass) private var sizeClass
    @ViewBuilder let content: Content
    var body: some View {
        if sizeClass == .compact { ScrollView { VStack(alignment: .leading, spacing: 16) { content } } }
        else { HStack(alignment: .top, spacing: 16) { content } }
    }
}
