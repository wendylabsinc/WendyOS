import XCTest
@testable import WendyMobile

final class MobileRecordingTests: XCTestCase {
    func testFilteredExportWaitsForMissingLabelsAndNeverIncludesAHiddenSelection() {
        let device = Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")
        func episode(_ id: String, _ reason: String?, loaded: Bool = true) -> MobileRecordingEpisode {
            MobileRecordingEpisode(id: id, title: id, started: nil, bytes: 100, duration: nil, trigger: reason,
                                   kind: .unknown, synced: nil, topics: [], detailsLoaded: loaded)
        }
        let motion = episode("first", "Motion"), other = episode("second", "Manual")
        let partial = MobileRecordingLibrary(device: device, model: nil,
                                            episodes: [motion, other, episode("third", nil, loaded: false)], warning: nil)
        XCTAssertEqual(partial.exportTargets(trigger: nil, selectedID: nil).count, 3)
        XCTAssertTrue(partial.exportTargets(trigger: "Motion", selectedID: nil).isEmpty)
        XCTAssertEqual(partial.exportTargets(trigger: "Motion", selectedID: motion.id).map(\.id), ["first"])
        XCTAssertTrue(partial.exportTargets(trigger: "Motion", selectedID: other.id).isEmpty)

        let complete = MobileRecordingLibrary(device: device, model: nil,
                                              episodes: [motion, other, episode("third", "Motion")], warning: nil)
        XCTAssertNil(complete.labelCoverage)
        XCTAssertEqual(complete.labels.map(\.name), ["Motion", "Manual"])
        XCTAssertEqual(complete.labels.map(\.count), [2, 1])
        XCTAssertEqual(complete.exportTargets(trigger: "Motion", selectedID: nil).map(\.id), ["first", "third"])
    }

    func testOnlyMatchingTenantAndDeviceIDsShareARecordingCard() {
        func route(_ id: String, _ transport: DeviceTransport) -> Device {
            Device(id: id, name: "Same name", selector: id, transport: transport, address: "same-address")
        }
        let nearby = route("spiffe://wendy.sh/tenant/first/device/asset-id", .lan)
        let cloud = route("cloud://api.wendy.dev:443/tenant/first/asset/asset-id", .cloud)
        let otherOrg = route("cloud://api.wendy.dev:443/tenant/second/asset/asset-id", .cloud)
        let unknown = route("speaker.local:50051", .lan)
        XCTAssertEqual(recordingDeviceID(nearby), recordingDeviceID(cloud))
        XCTAssertNotEqual(recordingDeviceID(nearby), recordingDeviceID(otherOrg))
        XCTAssertNotEqual(recordingDeviceID(unknown), recordingDeviceID(cloud))
    }
}
