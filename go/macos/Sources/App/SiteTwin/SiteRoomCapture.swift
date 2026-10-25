#if os(iOS)
    import SwiftUI
    import RoomPlan
    import AVFoundation
    import ARKit

    /// RoomPlan supplies metric room geometry. It is not a photorealistic scan;
    /// users can import a textured USDZ when appearance matters.
    @MainActor
    public struct SiteRoomCapture: View {
        public static var isSupported: Bool { RoomCaptureSession.isSupported }
        private let completion: (Result<URL, Error>) -> Void
        private let requiresVisualReferences: Bool
        @Environment(\.dismiss) private var dismiss
        @State private var controller = CaptureController()
        @State private var allowed = false
        @State private var denied = false

        public init(requiresVisualReferences: Bool = false, completion: @escaping (Result<URL, Error>) -> Void) {
            self.requiresVisualReferences = requiresVisualReferences
            self.completion = completion
        }

        public var body: some View {
            NavigationStack {
                Group {
                    if denied {
                        ContentUnavailableView(
                            "Camera Access Needed",
                            systemImage: "camera",
                            description: Text("Allow camera access in Settings to scan the room.")
                        )
                    } else if allowed {
                        CaptureSurface(controller: controller)
                            .task(id: controller.finishing) {
                                if !controller.finishing { await controller.vision.run(manageSession: false) }
                            }
                            .task(id: controller.finishing) {
                                if !controller.finishing { await controller.references.run(session: controller.vision.session) }
                            }
                            .overlay(alignment: .top) {
                                if let sight = controller.vision.sights.first {
                                    VStack(spacing: 4) {
                                        Label(sight.label, systemImage: "viewfinder").font(.headline)
                                        Text("Appearance recognized. Camera direction is not measured.").font(.caption)
                                    }.padding().background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16)).padding()
                                }
                            }
                    } else {
                        ProgressView("Opening Camera…")
                    }
                }
                .safeAreaInset(edge: .bottom) {
                    VStack(spacing: 6) {
                        Text("Walk around the area visible to your fixed camera. Include textured furniture and surfaces at different depths.")
                        Text(controller.references.message).font(.caption)
                    }
                    .font(.callout).padding().background(.regularMaterial)
                }
                .navigationTitle("Scan Room")
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") {
                            controller.cancel()
                            dismiss()
                        }
                    }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Finish") { controller.finish() }.disabled(
                            !allowed || controller.finishing || (requiresVisualReferences && controller.references.landmarkCount < 120)
                        )
                    }
                }
                .overlay {
                    if controller.finishing {
                        ProgressView("Building Room…").padding().background(.regularMaterial)
                    }
                }
            }
            .task {
                
                let permission = await AVCaptureDevice.requestAccess(for: .video)
                guard !Task.isCancelled else { return }
                allowed = permission
                denied = !permission
                guard permission else { return }
                do {
                    let room = try await controller.result.value()
                    try Task.checkCancellation()
                    let folder = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
                    try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
                    do {
                        let url = folder.appendingPathComponent("Room.usdz")
                        try room.export(to: url, exportOptions: .mesh)
                        let model = try Data(contentsOf: url)
                        let site = Site(name: "Room")
                        var visualMap: SiteVisualMap?
                        if controller.references.landmarkCount >= 120 {
                            var transforms = Dictionary(uniqueKeysWithValues: room.walls.map { ($0.identifier, $0.transform) })
                            for surface in room.floors + room.doors + room.windows + room.openings { transforms[surface.identifier] = surface.transform }
                            for object in room.objects { transforms[object.identifier] = object.transform }
                            try SiteScanCoordinates.verify(model: url, transforms: transforms)
                            visualMap = try await controller.references.snapshot(model: model, revision: site.modelRevision)
                        } else if requiresVisualReferences { throw SiteLocalizationError.insufficientMap }
                        let archive = try SiteArchive(site: site, model: model, visualMap: visualMap)
                        let archiveURL = folder.appendingPathComponent("Room.wendysite")
                        try archive.data().write(to: archiveURL, options: .atomic)
                        completion(.success(archiveURL))
                    } catch {
                        try? FileManager.default.removeItem(at: folder)
                        throw error
                    }
                } catch is CancellationError {} catch { completion(.failure(error)) }
            }
            .onDisappear { controller.cancel() }
        }
    }

    @Observable @MainActor
    @objc(WendySiteCaptureController)
    private final class CaptureController: NSObject, RoomCaptureViewDelegate {
        let vision = DeviceViewfinderController()
        let references = SiteReferenceCapture()
        var finishing = false
        nonisolated let result = AsyncCallback<CapturedRoom>()
        var view: RoomCaptureView?
        private var cancelled = false

        override init() { super.init() }
        nonisolated required init?(coder: NSCoder) { nil }
        nonisolated func encode(with coder: NSCoder) {}

        func finish() {
            finishing = true
            view?.captureSession.stop()
        }
        func cancel() {
            cancelled = true
            result.finish(.failure(CancellationError()))
            view?.captureSession.stop()
            view?.delegate = nil
        }

        nonisolated func captureView(
            shouldPresent roomDataForProcessing: CapturedRoomData,
            error: Error?
        ) -> Bool {
            guard let error else { return true }
            result.finish(.failure(error))
            return false
        }

        nonisolated func captureView(didPresent processedResult: CapturedRoom, error: Error?) {
            if let error { result.finish(.failure(error)) }
            else { result.finish(.success(processedResult)) }
        }
    }

    private struct CaptureSurface: UIViewRepresentable {
        let controller: CaptureController
        func makeUIView(context: Context) -> RoomCaptureView {
            let view = RoomCaptureView(frame: .zero, arSession: controller.vision.session)
            controller.view = view
            view.delegate = controller
            view.captureSession.run(configuration: .init())
            if ARWorldTrackingConfiguration.supportsFrameSemantics(.sceneDepth),
               let configuration = controller.vision.session.configuration?.copy() as? ARWorldTrackingConfiguration {
                configuration.frameSemantics.insert(.sceneDepth)
                controller.vision.session.run(configuration)
            }
            return view
        }
        func updateUIView(_ view: RoomCaptureView, context: Context) {}
        static func dismantleUIView(_ view: RoomCaptureView, coordinator: ()) {
            view.captureSession.stop()
            view.delegate = nil
        }
    }
#endif
