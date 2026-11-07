import XCTest
@testable import WendyMobile

final class RememberedAIDeviceTests: XCTestCase {
    @MainActor func testVerifiedDiscoveryPersistsMetadataAndEmptyDiscoveryRemovesDevice() {
        let name = "RememberedAI." + UUID().uuidString
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let store = RememberedAIDevices(defaults: defaults)
        let device = Device(id: "board", name: "Board", selector: "secret-endpoint", transport: .lan, address: "")
        let models = [ChatModelOption(id: "llama", name: "Llama", port: 1234), ChatModelOption(id: "disabled", name: "Disabled", unavailableReason: "Unavailable")]
        store.discovered(device: device, models: models, account: "alice", organization: "one", now: Date(timeIntervalSince1970: 100))
        store.used(deviceID: "board", model: "llama", account: "alice", organization: "one", now: Date(timeIntervalSince1970: 200))
        let loaded = RememberedAIDevices(defaults: defaults)
        let rows = loaded.records(account: "alice", organization: "one")
        XCTAssertEqual(rows.count, 1)
        XCTAssertEqual(rows[0].models.map(\.id), ["llama"])
        XCTAssertEqual(rows[0].preferred?.port, 1234)
        XCTAssertEqual(rows[0].lastUsed, Date(timeIntervalSince1970: 200))
        XCTAssertTrue(loaded.records(account: "bob", organization: "one").isEmpty)
        XCTAssertTrue(loaded.records(account: "alice", organization: "two").isEmpty)
        loaded.discovered(device: device, models: [], account: "alice", organization: "one")
        XCTAssertTrue(loaded.records(account: "alice", organization: "one").isEmpty)
    }

    @MainActor func testOnlySuccessfulDeviceDiscoveryNotifiesMemory() async {
        var discoveries = 0
        let success = DeviceChatModel(session: 1, discoverModels: { _, _ in [ChatModelOption(id: "a", name: "A")] })
        success.onDeviceDiscovery = { _ in discoveries += 1 }
        await success.discover()
        XCTAssertEqual(discoveries, 1)
        let failed = DeviceChatModel(session: 1, discoverModels: { _, _ in throw WendyError(message: "Offline") })
        failed.onDeviceDiscovery = { _ in discoveries += 1 }
        await failed.discover()
        XCTAssertEqual(discoveries, 1)
        let empty = DeviceChatModel(session: 1, discoverModels: { _, _ in [] })
        empty.onDeviceDiscovery = { result in XCTAssertTrue(result.isEmpty); discoveries += 1 }
        await empty.discover()
        XCTAssertEqual(discoveries, 2)
    }
    @MainActor func testSuggestionVerificationCannotUseStaleModelAfterFailure() async {
        let chat = DeviceChatModel(session: 1, discoverModels: { _, _ in throw WendyError(message: "Offline") })
        chat.useModel("old", port: 11434)
        chat.prompt = "Hello"
        await chat.verifySuggestedModel("old", port: 11434)
        XCTAssertTrue(chat.models.isEmpty)
        XCTAssertTrue(chat.model.isEmpty)
        XCTAssertFalse(chat.canSend)
        XCTAssertNotNil(chat.error)
    }

    @MainActor func testCancelledDiscoveryDoesNotRememberModels() async {
        var discoveries = 0
        let chat = DeviceChatModel(session: 1, discoverModels: { _, _ in
            try await Task.sleep(for: .milliseconds(200))
            return [ChatModelOption(id: "a", name: "A")]
        })
        chat.onDeviceDiscovery = { _ in discoveries += 1 }
        await withTaskGroup(of: Void.self) { group in
            group.addTask { await chat.discover() }
            while !chat.loading { await Task.yield() }
            group.cancelAll()
            await group.waitForAll()
        }
        XCTAssertEqual(discoveries, 0)
        XCTAssertTrue(chat.models.isEmpty)
        XCTAssertFalse(chat.loading)
    }

}
