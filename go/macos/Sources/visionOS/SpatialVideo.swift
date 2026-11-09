import SwiftUI
import AVFoundation
import QuartzCore

private final class SpatialVideoHost: UIView {
    let display = AVSampleBufferDisplayLayer()
    override init(frame: CGRect) {
        super.init(frame: frame)
        backgroundColor = .black
        layer.addSublayer(display)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) is unavailable") }
    override func layoutSubviews() { super.layoutSubviews(); display.frame = bounds }
}
private struct SpatialVideoSurface: UIViewRepresentable {
    let decoder: H264AnnexBDecoder
    func makeCoordinator() -> H264AnnexBDecoder { decoder }
    func makeUIView(context: Context) -> UIView {
        let view = SpatialVideoHost()
        decoder.attach(layer: view.display)
        return view
    }
    func updateUIView(_ uiView: UIView, context: Context) {}
    static func dismantleUIView(_ uiView: UIView, coordinator: H264AnnexBDecoder) {
        if let view = uiView as? SpatialVideoHost { coordinator.detach(layer: view.display) }
    }
}

struct SpatialVideoView: View {
    let model: SpatialModel
    @State private var decoder = H264AnnexBDecoder()
    @State private var received: TimeInterval = 0
    @State private var failure: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label("\(model.robot?.name ?? "Go2") camera", systemImage: "video")
            ZStack {
                SpatialVideoSurface(decoder: decoder).aspectRatio(16 / 9, contentMode: .fit)
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    if CACurrentMediaTime() - received > 3 {
                        Text(failure ?? "Waiting for live video…").padding().glassBackgroundEffect()
                    }
                }
            }.clipShape(RoundedRectangle(cornerRadius: 16))
        }
        .task(id: "\(model.handle)-\(model.active)") {
            decoder.reset(); received = 0; failure = nil
            let handle = model.handle
            guard handle != 0, model.active else { return }
            while !Task.isCancelled, handle == model.handle, model.active {
                do {
                    let frame: Data = try await SpatialClient.work {
                        guard let result = WDSiteRobotVideoNext(handle) else { throw SpatialFailure("Camera unavailable.") }
                        defer { WDUnitreeVideoFrameFree(result) }
                        try SpatialClient.check(result.pointee.status)
                        guard let bytes = result.pointee.bytes, result.pointee.count > 0 else { return Data() }
                        return Data(bytes: bytes, count: result.pointee.count)
                    }
                    guard !Task.isCancelled, handle == model.handle else { break }
                    if !frame.isEmpty { decoder.feed(frame); received = CACurrentMediaTime() }
                } catch { failure = error.localizedDescription; break }
            }
            decoder.reset(); received = 0
        }
    }
}
