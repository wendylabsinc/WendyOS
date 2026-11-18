import XCTest
@testable import WendyMobile

final class MobileNotificationTests: XCTestCase {
    func testNotificationGuidanceRemainsReadableWithoutExposingUnknownErrors() {
        let message = "Switch to this alert’s organization in Settings to open it."
        XCTAssertEqual(MobileNotificationMessage.display(WendyError(message: message)), message)
        XCTAssertEqual(MobileNotificationMessage.display(WendyError(message: "internal server details")), "Wendy could not complete this request. Try again.")
    }
    let org = "11111111-1111-1111-1111-111111111111"
    let event = "22222222-2222-2222-2222-222222222222"
    let deviceID = "33333333-3333-3333-3333-333333333333"
    func testPushReferenceAndDestinationValidateOrganizationAndEvent() throws {
        let reference = try XCTUnwrap(MobilePushReference(userInfo: ["wendy": ["version": 2, "notification_id": event, "organization_id": org]]))
        var row = MobileCloudNotification(id: event, date: Date(), title: "Person", body: "", organizationID: org,
            deepLink: "wendy://organizations/\(org)/devices/\(deviceID)/live?camera=front", recordingEventID: event, recordingCampaign: "person")
        let destination = try MobileNotificationDestination(notification: row, reference: reference)
        XCTAssertEqual(destination.deviceID, deviceID)
        XCTAssertEqual(destination.cameraID, "front")
        XCTAssertEqual(destination.recordingEventID, event)
        row.organizationID = deviceID
        XCTAssertThrowsError(try MobileNotificationDestination(notification: row, reference: reference))
        row.organizationID = org; row.recordingEventID = deviceID
        XCTAssertThrowsError(try MobileNotificationDestination(notification: row, reference: reference))
        row.recordingEventID = event; row.deepLink += "&camera=other"
        XCTAssertThrowsError(try MobileNotificationDestination(notification: row, reference: reference))
        XCTAssertNil(MobilePushReference(userInfo: ["wendy": ["version": 1, "notification_id": event, "organization_id": org]]))
        XCTAssertTrue(MobileNotificationTap(userInfo: ["wendy": "invalid"]).malformedCloudReference)
    }
    @MainActor func testOlderTapCannotConsumeNewerTap() {
        let routing = MobileNotificationRouting()
        let first = MobileNotificationTap(userInfo: [:]), second = MobileNotificationTap(userInfo: [:])
        routing.receive(first); routing.receive(second); routing.finish(first)
        XCTAssertEqual(routing.pending?.id, second.id)
    }
    func testPendingRecordingRetriesAndStopsAtLimit() async throws {
        let device = Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")
        let fixture = PendingNotificationFixture()
        let row = try await MobileNotificationRecording.find(device: device, campaign: "person", eventID: event,
            read: { _, _, _ in await fixture.next() }, pause: {})
        XCTAssertEqual(row.id, "exact-event")
        let count = await fixture.count
        XCTAssertEqual(count, 3)
        do {
            _ = try await MobileNotificationRecording.find(device: device, campaign: "person", eventID: event, attempts: 2, read: { _, _, _ in nil }, pause: {})
            XCTFail("A missing clip must time out")
        } catch { XCTAssertTrue(error.localizedDescription.contains("still being saved")) }
    }
    func testCameraRefreshPreservesRowsButRejectsAmbiguousAndMissingChoices() {
        var inventory = CameraPickerInventory()
        let one = Camera(id: 1, name: "Front", thermal: false, stableID: "front")
        let two = Camera(id: 2, name: "Back", thermal: false, stableID: "back")
        inventory.merge([one, two]); inventory.merge([one])
        XCTAssertEqual(inventory.cameras.count, 2)
        XCTAssertEqual(inventory.available, ["front"])
        inventory.merge([one, one])
        XCTAssertTrue(inventory.available.isEmpty)
        XCTAssertEqual(inventory.cameras.count, 2)
    }
}
private actor PendingNotificationFixture {
    private(set) var count = 0
    func next() -> MobileRecordingEpisode? {
        count += 1
        guard count == 3 else { return nil }
        return MobileRecordingEpisode(id: "exact-event", title: "Person", started: nil, bytes: 1, duration: 1, trigger: "Person", kind: .video, synced: nil, topics: [])
    }
}
