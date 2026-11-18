import Foundation

// The picker owns remembered rows. Search only changes visibility; a refresh
// updates availability without deleting choices the user was just considering.
struct CameraPickerInventory {
    private(set) var cameras: [Camera] = []
    private(set) var available: Set<String> = []
    mutating func merge(_ incoming: [Camera]) {
        let groups = Dictionary(grouping: incoming, by: \.stableID)
        available = Set(groups.filter { !$0.key.isEmpty && $0.value.count == 1 }.keys)
        for camera in incoming where available.contains(camera.stableID) {
            if let index = cameras.firstIndex(where: { $0.stableID == camera.stableID }) { cameras[index] = camera }
            else { cameras.append(camera) }
        }
    }
}


extension GoClient {
    // A picker refresh must query inventory even when detail navigation already
    // has this device connected. Its temporary session never replaces that view.
    func cameraChoices(device: Device) async throws -> [Camera] {
        let queue = DispatchQueue(label: "dev.wendy.mobile.camera-choices", qos: .userInitiated)
        let handle = WDPreviewCreate()
        guard handle != 0 else { throw WendyError(message: "Camera access is paused. Try again.") }
        let result: Result<[Camera], Error> = await withTaskCancellationHandler {
            do {
                return .success(try await BlockingWork.run(on: queue) {
                    try Self.withCString(device.selector) { try Self.readCameras(WDPreviewCameras(handle, $0, device.requiresMTLS ? 1 : 0)) }
                })
            } catch { return .failure(error) }
        } onCancel: { WDPreviewCancel(handle) }
        _ = try? await BlockingWork.run(on: queue, unlessCancelled: false) { WDPreviewClose(handle) }
        try Task.checkCancellation()
        return try result.get()
    }
}
