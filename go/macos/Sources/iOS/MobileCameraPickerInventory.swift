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
