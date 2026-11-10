import XCTest
@testable import WendyMobile

@MainActor private final class MemoryChatCredentials: ChatCredentialStore {
    var values: [ChatCloudProvider: String] = [:]
    func read(_ provider: ChatCloudProvider) throws -> String? { values[provider] }
    func save(_ key: String, for provider: ChatCloudProvider) throws { values[provider] = key }
    func remove(_ provider: ChatCloudProvider) throws { values[provider] = nil }
}

final class CloudChatTests: XCTestCase {
    @MainActor func testKeychainSaveReplaceRemoveKeepsProvidersSeparate() throws {
        let store = ChatKeychain(service: "dev.wendy.tests.cloud-chat.\(UUID().uuidString)")
        defer { try? store.remove(.openai); try? store.remove(.anthropic) }
        XCTAssertNil(try store.read(.openai))
        try store.save("fake-first", for: .openai)
        try store.save("fake-anthropic", for: .anthropic)
        try store.save("fake-replacement", for: .openai)
        XCTAssertEqual(try store.read(.openai), "fake-replacement")
        XCTAssertEqual(try store.read(.anthropic), "fake-anthropic")
        try store.remove(.openai)
        XCTAssertNil(try store.read(.openai))
        XCTAssertEqual(try store.read(.anthropic), "fake-anthropic")
    }

    @MainActor func testInferenceSwitchClearsPrivateHistoryAndPendingAction() {
        let credentials = MemoryChatCredentials()
        let chat = DeviceChatModel(session: 3, credentials: credentials)
        chat.useModel("local-model", port: 11434)
        chat.prompt = "Read local logs"
        chat.send()
        chat.approval = ChatApproval(id: 7, action: "container_stop", app: "test")
        chat.selectInference(.cloud)
        XCTAssertTrue(chat.messages.isEmpty)
        XCTAssertEqual(chat.turn, 0)
        XCTAssertNil(chat.approval)
        XCTAssertFalse(chat.streaming)
        XCTAssertTrue(chat.models.isEmpty)
        XCTAssertFalse(chat.canSend)
        XCTAssertTrue(chat.saveCloudKey("fake-openai-key"))
        chat.models = [ChatModelOption(id: "cloud-model", name: "Cloud model", port: 0)]
        chat.model = "cloud-model"
        chat.port = "not-a-device-port"
        chat.prompt = "Which apps are running?"
        XCTAssertTrue(chat.canSend, "Cloud inference must not depend on a device model port")
        chat.selectProvider(.anthropic)
        XCTAssertFalse(chat.hasCloudKey)
        XCTAssertTrue(chat.models.isEmpty)
        XCTAssertFalse(chat.canSend)
        XCTAssertEqual(credentials.values[.openai], "fake-openai-key")
        chat.selectProvider(.openai)
        XCTAssertTrue(chat.hasCloudKey)
        chat.removeCloudKey()
        XCTAssertNil(credentials.values[.openai])
        XCTAssertFalse(chat.hasCloudKey)
        chat.selectInference(.device)
        XCTAssertFalse(chat.canSend, "Switching back must not reuse cloud models")
    }

    @MainActor func testLateLocalDiscoveryCannotPopulateCloudSelection() async {
        var resume: CheckedContinuation<[ChatModelOption], Never>?
        let chat = DeviceChatModel(session: 3, credentials: MemoryChatCredentials(), discoverModels: { _, _ in
            await withCheckedContinuation { resume = $0 }
        })
        async let discovery: Void = chat.discover()
        while resume == nil { await Task.yield() }
        chat.selectInference(.cloud)
        resume?.resume(returning: [ChatModelOption(id: "private-local-model", name: "Local", port: 11434)])
        await discovery
        XCTAssertEqual(chat.inference, .cloud)
        XCTAssertTrue(chat.models.isEmpty)
        XCTAssertTrue(chat.model.isEmpty)
        XCTAssertFalse(chat.loading)
    }
}
