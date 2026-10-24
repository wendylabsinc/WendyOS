import SwiftUI
import UIKit
import AVFoundation
import SafariServices

// The app can access devices directly without installing a system-wide VPN.
// iOS VPN requires a separately provisioned packet-tunnel app extension.
@MainActor final class VPNController: ObservableObject {
    let setupIssue: String? = "System-wide VPN is not included in this iOS build. Cloud connections work inside Wendy."
    func start(_ configuration: VPNConfiguration) async throws { throw WendyError(message: setupIssue!) }
    func stop() async throws {}
}
struct BrowserSheet: UIViewControllerRepresentable {
    let url: URL
    func makeUIViewController(context: Context) -> SFSafariViewController { SFSafariViewController(url: url) }
    func updateUIViewController(_ controller: SFSafariViewController, context: Context) {}
}
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
