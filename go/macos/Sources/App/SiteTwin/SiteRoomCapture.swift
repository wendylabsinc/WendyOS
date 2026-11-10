#if os(iOS)
    import SwiftUI
    import RoomPlan
    import AVFoundation
    import ARKit
    import Synchronization

    /// RoomPlan supplies metric room geometry. It is not a photorealistic scan;
    /// users can import a textured USDZ when appearance matters.
    @MainActor
    public struct SiteRoomCapture: View {
        public static var isSupported: Bool { RoomCaptureSession.isSupported }
        private let completion: (Result<URL, Error>) -> Void
        private let requiresVisualReferences: Bool
        @Environment(\.dismiss) private var dismiss
        // StateObject defers construction until SwiftUI installs the view. An
        // eager @State initializer creates discarded ARSessions on view updates.
        @StateObject private var controller = SiteRoomCaptureController()
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
                            .task(id: controller.scanning) {
                                if controller.scanning { await controller.references.run(session: controller.session) }
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
                            !allowed || !controller.scanning || (requiresVisualReferences && controller.references.landmarkCount < 120)
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
            .onReceive(NotificationCenter.default.publisher(for: UIApplication.didReceiveMemoryWarningNotification)) { _ in
                controller.memoryWarning()
            }
            .onDisappear { controller.cancel() }
        }
    }

    protocol SiteRoomCaptureSession: AnyObject {
        func run(configuration: RoomCaptureSession.Configuration)
        func stop()
    }

    extension RoomCaptureSession: SiteRoomCaptureSession {}

    @MainActor
    @objc(WendySiteCaptureController)
    final class SiteRoomCaptureController: NSObject, ObservableObject, RoomCaptureViewDelegate {
        // RoomPlan and reference capture share the camera. Device recognition has
        // its own large image-processing buffers and is unnecessary during a scan.
        let session = ARSession()
        let references = SiteReferenceCapture()
        @Published private(set) var finishing = false
        @Published private(set) var scanning = true
        nonisolated let result = AsyncCallback<CapturedRoom>()
        private var captureSession: (any SiteRoomCaptureSession)?
        nonisolated private let processingAllowed = Mutex(true)

        override init() { super.init() }
        nonisolated required init?(coder: NSCoder) { nil }
        nonisolated func encode(with coder: NSCoder) {}

        @discardableResult
        func start(_ captureSession: any SiteRoomCaptureSession) -> Bool {
            guard scanning, self.captureSession == nil else { return false }
            self.captureSession = captureSession
            captureSession.run(configuration: .init())
            return true
        }

        private func stopCapture() {
            // Finish, Cancel, onDisappear and dismantleUIView can all arrive for
            // one scan. RoomPlan owns the AR session pause; stop it exactly once.
            let active = captureSession
            captureSession = nil
            active?.stop()
        }

        func finish() {
            guard scanning else { return }
            scanning = false
            finishing = true
            references.stop()
            stopCapture()
        }

        func memoryWarning() {
            // Starting RoomPlan's final reconstruction here would allocate more
            // memory at the point iOS has asked us to release it.
            guard scanning else { return }
            stop(with: SiteRoomCaptureError.memoryPressure)
        }

        func cancel() {
            stop(with: CancellationError())
        }

        private func stop(with error: Error) {
            processingAllowed.withLock { $0 = false }
            scanning = false
            finishing = false
            references.stop()
            result.finish(.failure(error))
            // Keep the delegate installed to reject RoomPlan's queued processing
            // callback. With no delegate, the view may process the room by default.
            stopCapture()
        }

        nonisolated func shouldProcess(error: Error?) -> Bool {
            guard processingAllowed.withLock({ $0 }) else { return false }
            guard let error else { return true }
            result.finish(.failure(error))
            return false
        }

        nonisolated func captureView(
            shouldPresent roomDataForProcessing: CapturedRoomData,
            error: Error?
        ) -> Bool {
            shouldProcess(error: error)
        }

        nonisolated func captureView(didPresent processedResult: CapturedRoom, error: Error?) {
            if let error { result.finish(.failure(error)) }
            else { result.finish(.success(processedResult)) }
        }
    }

    enum SiteRoomCaptureError: LocalizedError {
        case memoryPressure

        var errorDescription: String? {
            "The room scan stopped because this device is low on memory. Scan a smaller area and tap Finish sooner. This scan was not saved."
        }
    }

    private struct CaptureSurface: UIViewRepresentable {
        let controller: SiteRoomCaptureController
        func makeCoordinator() -> SiteRoomCaptureController { controller }
        func makeUIView(context: Context) -> RoomCaptureView {
            let view = RoomCaptureView(frame: .zero, arSession: controller.session)
            view.delegate = controller
            guard controller.start(view.captureSession) else { return view }
            if ARWorldTrackingConfiguration.supportsFrameSemantics(.sceneDepth),
               let configuration = controller.session.configuration?.copy() as? ARWorldTrackingConfiguration {
                configuration.frameSemantics.insert(.sceneDepth)
                controller.session.run(configuration)
            }
            return view
        }
        func updateUIView(_ view: RoomCaptureView, context: Context) {}
        static func dismantleUIView(_ view: RoomCaptureView, coordinator: SiteRoomCaptureController) {
            coordinator.cancel()
        }
    }
#endif
