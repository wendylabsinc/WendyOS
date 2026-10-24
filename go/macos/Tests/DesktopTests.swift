import XCTest
@testable import WendyDesktop

final class DesktopTests: XCTestCase {
    func testNotificationApprovalUsesReadableConsent() {
        let approval = ChatApproval(id: 42, action: "enable_campaign_notifications", app: #"{"campaign":"door-people"}"#)
        XCTAssertTrue(approval.isNotificationPermission)
        XCTAssertEqual(approval.title, "Allow campaign notifications?")
        XCTAssertEqual(approval.confirmLabel, "Allow notifications")
        XCTAssertEqual(approval.campaignName, "door-people")
        XCTAssertFalse(ChatApproval(id: 43, action: "container_stop", app: "{}").isNotificationPermission)
    }

    func testCampaignApprovalSummarizesExactScope() {
        let approval = ChatApproval(id: 1, action: "create_person_detection_campaign", app: #"{"name":"person-detection","camera":"*","before_seconds":10,"after_seconds":15,"notify":true,"upload":false}"#)
        XCTAssertTrue(approval.isCampaign)
        XCTAssertEqual(approval.confirmLabel, "Create campaign")
        XCTAssertEqual(approval.campaignName, "person-detection")
        XCTAssertEqual(approval.cameraName, "All current and future cameras")
        XCTAssertEqual(approval.clipSummary, "10s before · 15s after detection")
    }

    @MainActor
    func testChatSurvivesPaneRecreationAndRediscovery() async {
        let app = AppModel()
        let chat = app.deviceChat
        chat.useModel("qwen3:4b", port: 11434)
        chat.messages = [ChatMessage(isUser: true, text: "Remember the front camera"),
                         ChatMessage(isUser: false, text: "Front camera selected")]
        chat.prompt = "Show it again"
        _ = DeviceChatView(deviceName: "Thor", chat: app.deviceChat)
        // Reopening the tab must not query the server or reset the selected model.
        await chat.discoverIfNeeded()
        _ = DeviceChatView(deviceName: "Thor", chat: app.deviceChat)
        XCTAssertTrue(app.deviceChat === chat)
        XCTAssertEqual(chat.messages.map(\.text), ["Remember the front camera", "Front camera selected"])
        XCTAssertEqual(chat.prompt, "Show it again")
        XCTAssertEqual(chat.model, "qwen3:4b")
        XCTAssertNil(chat.error)
        XCTAssertTrue(chat.canSend)
        chat.clear()
        XCTAssertTrue(chat.messages.isEmpty)
    }

    @MainActor
    func testCancelledChatResponseDoesNotSpinDuringNextTurn() {
        let chat = DeviceChatModel(session: 0)
        chat.useModel("qwen3:4b", port: 11434)
        chat.prompt = "First question"
        chat.send()
        let first = chat.messages.last!
        XCTAssertTrue(chat.isWaiting(for: first))
        chat.stop()
        XCTAssertTrue(chat.messages.last!.interrupted)
        XCTAssertFalse(chat.isWaiting(for: first))
        XCTAssertNil(chat.activeResponseID)
        chat.prompt = "Second question"
        chat.send()
        XCTAssertFalse(chat.isWaiting(for: first))
        XCTAssertTrue(chat.isWaiting(for: chat.messages.last!))
        chat.stop()
        XCTAssertFalse(chat.streaming)
        XCTAssertTrue(chat.activity.isEmpty)
    }

    func testChatMarkdownKeepsCodeLiteralAndSeparatesLists() {
        let blocks = ChatMarkdownBlock.parse("## Devices\n\n- **Thor**\n  - `192.168.1.2`\n\n```sh\n# literal comment\necho '**not bold**'\n```")
        XCTAssertEqual(blocks.map(\.kind), [.heading(2), .item("•", 0), .item("•", 1), .code])
        XCTAssertEqual(blocks.last?.text, "# literal comment\necho '**not bold**'")
        let partial = ChatMarkdownBlock.parse("```swift\nlet count = 1")
        XCTAssertEqual(partial.first?.kind, .code)
        XCTAssertEqual(partial.first?.text, "let count = 1")
    }

    func testChatMarkdownRecognizesTablesAndOrderedLists() {
        let blocks = ChatMarkdownBlock.parse("| Device | Status |\n| --- | :---: |\n| Thor | Ready |\n\n1. Inspect\n2. Connect")
        XCTAssertEqual(blocks.map(\.kind), [.table, .item("1.", 0), .item("2.", 0)])
        XCTAssertEqual(ChatMarkdownBlock.tableCells("| Thor | Ready |"), ["Thor", "Ready"])
    }

    @MainActor
    func testSelectionChangesBeforeConnectionAndKeepsFailureOnSelectedDevice() async {
        let old = Device(id: "old", name: "Old", selector: "old", transport: .lan, address: "old")
        let next = Device(id: "next", name: "Next", selector: "next", transport: .cloud, address: "next")
        let started = expectation(description: "Connection started")
        var pending: CheckedContinuation<UInt64, Error>?
        let model = AppModel { _, _, _ in
            try await withCheckedThrowingContinuation { continuation in
                pending = continuation
                started.fulfill()
            }
        }
        model.selection = old; model.connected = old
        let task = Task { await model.connect(next) }
        await fulfillment(of: [started], timeout: 1)
        XCTAssertEqual(model.selection?.id, next.id)
        XCTAssertEqual(model.connectingDevice?.id, next.id)
        XCTAssertFalse(model.selectedDeviceReady)
        pending?.resume(throwing: WendyError(message: "Device offline"))
        await task.value
        XCTAssertEqual(model.selection?.id, next.id)
        XCTAssertEqual(model.detailError, "Device offline")
        XCTAssertFalse(model.selectedDeviceReady)
        await model.connect(old)
        XCTAssertTrue(model.selectedDeviceReady)
        XCTAssertNil(model.detailError)
    }

    @MainActor
    func testLateConnectionFailureCannotReplaceNewSelection() async {
        let old = Device(id: "old", name: "Old", selector: "old", transport: .lan, address: "old")
        let next = Device(id: "next", name: "Next", selector: "next", transport: .lan, address: "next")
        let started = expectation(description: "Connection started")
        var pending: CheckedContinuation<UInt64, Error>?
        let model = AppModel { _, _, _ in
            try await withCheckedThrowingContinuation { continuation in
                pending = continuation
                started.fulfill()
            }
        }
        model.selection = old; model.connected = old
        let task = Task { await model.connect(next) }
        await fulfillment(of: [started], timeout: 1)
        await model.connect(old)
        pending?.resume(throwing: WendyError(message: "Late failure"))
        await task.value
        XCTAssertEqual(model.selection?.id, old.id)
        XCTAssertTrue(model.selectedDeviceReady)
        XCTAssertNil(model.detailError)
    }

    @MainActor
    func testBatchStopContinuesAfterFailureAndDeduplicates() async {
        var stopped: [String] = []
        let errors = await AppStopBatch.run(names: ["c", "a", "b", "a"], isCurrent: { true }) { name in
            stopped.append(name)
            if name == "b" { throw NSError(domain: "Test", code: 1, userInfo: [NSLocalizedDescriptionKey: "stop failed"]) }
        }
        XCTAssertEqual(stopped, ["a", "b", "c"])
        XCTAssertEqual(errors, ["b: stop failed"])
    }

    @MainActor
    func testBatchStopDoesNotContinueAfterDeviceChange() async {
        var current = true
        var stopped: [String] = []
        let errors = await AppStopBatch.run(names: ["a", "b"], isCurrent: { current }) { name in
            stopped.append(name)
            current = false
        }
        XCTAssertEqual(stopped, ["a"])
        XCTAssertTrue(errors.isEmpty)
    }

    func testDevicePageKeyboardNavigationWrapsAndHandlesUnknownPage() {
        XCTAssertEqual(DesktopDevicePage.adjacent(to: "apps", forward: false), .settings)
        XCTAssertEqual(DesktopDevicePage.adjacent(to: "settings", forward: true), .apps)
        XCTAssertEqual(DesktopDevicePage.adjacent(to: "chat", forward: true), .data)
        XCTAssertEqual(DesktopDevicePage.adjacent(to: "chat", forward: false), .campaigns)
        XCTAssertEqual(DesktopDevicePage.adjacent(to: "removed-page", forward: true), .apps)
    }

    func testKeyboardSelectionFollowsVisibleOrderAndStopsAtEdges() {
        let ids = ["c", "a", "b"]
        XCTAssertEqual(KeyboardRowSelection.next(in: ids, selected: nil, down: true), "c")
        XCTAssertEqual(KeyboardRowSelection.next(in: ids, selected: nil, down: false), "b")
        XCTAssertEqual(KeyboardRowSelection.next(in: ids, selected: "c", down: false), "c")
        XCTAssertEqual(KeyboardRowSelection.next(in: ids, selected: "c", down: true), "a")
        XCTAssertEqual(KeyboardRowSelection.next(in: ids, selected: "b", down: true), "b")
        XCTAssertEqual(KeyboardRowSelection.next(in: ids, selected: "filtered-out", down: true), "c")
        XCTAssertNil(KeyboardRowSelection.next(in: [], selected: "a", down: true))
    }

    @MainActor
    func testCloudSaveDismissesWhileBusyAndRejectsDuplicateSubmission() async {
        let model = CloudManagementModel()
        var operations = 0
        var dismissed = false
        let saved = await model.save({
            operations += 1
            let duplicate = await model.save { operations += 1 }
            XCTAssertFalse(duplicate)
        }, onSuccess: {
            XCTAssertTrue(model.saving)
            dismissed = true
        })
        XCTAssertTrue(saved)
        XCTAssertTrue(dismissed)
        XCTAssertEqual(operations, 1)
        XCTAssertFalse(model.saving)
        XCTAssertNil(model.actionError)
    }

    @MainActor
    func testCloudSaveFailureKeepsFormOpenAndAllowsRetry() async {
        let model = CloudManagementModel()
        var dismissed = false
        let failed = await model.save({
            throw WendyError(message: "Could not save")
        }, onSuccess: { dismissed = true })
        XCTAssertFalse(failed)
        XCTAssertFalse(dismissed)
        XCTAssertFalse(model.saving)
        XCTAssertEqual(model.actionError, "Could not save")
        let retried = await model.save({}, onSuccess: { dismissed = true })
        XCTAssertTrue(retried)
        XCTAssertTrue(dismissed)
        XCTAssertNil(model.actionError)
    }

    @MainActor
    func testCameraWallKeepsTileStateAndGenerationsIndependent() throws {
        let first = CameraFeed(camera: Camera(id: 96, name: "ESP one"))
        let second = CameraFeed(camera: Camera(id: 97, name: "ESP two"))
        first.start(stream: 10)
        second.start(stream: 11)
        let failure = BridgeEvent(session: 1, stream: 10, payload: .cameraError("camera offline"))
        first.receive(failure)
        second.receive(failure)
        XCTAssertEqual(first.error, "camera offline")
        XCTAssertFalse(first.running)
        XCTAssertNil(second.error)
        XCTAssertTrue(second.running)
        first.start(stream: 12)
        first.receive(failure)
        XCTAssertNil(first.error)
        XCTAssertTrue(first.running)
        first.pause()
        first.fail("late failure", stream: 12)
        XCTAssertNil(first.error)
        XCTAssertEqual(first.stream, 0)
        XCTAssertTrue(second.running)
    }

    func testAnnexBAtEveryNetworkBoundary() {
        let bytes: [UInt8] = [0, 0, 0, 1, 0x67, 0x42, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x80, 0, 0, 1, 0x09]
        for split in 0...bytes.count {
            var parser = AnnexBParser()
            let first = parser.append(Data(bytes.prefix(split)))
            let second = parser.append(Data(bytes.dropFirst(split)))
            XCTAssertEqual(first + second, [Data([0x67, 0x42]), Data([0x68, 0xce]), Data([0x65, 0x80])], "split at \(split)")
        }
    }
    func testParserRecoversAfterOversizedNAL() {
        var parser = AnnexBParser()
        _ = parser.append(Data([0, 0, 1, 0x65]) + Data(repeating: 1, count: 16 * 1024 * 1024))
        XCTAssertEqual(parser.append(Data([0, 0, 1, 0x67, 1, 0, 0, 1, 0x68])), [Data([0x67, 1])])
    }
    private func allocate<T>(_ type: T.Type, count: Int = 1) -> UnsafeMutablePointer<T> {
        calloc(count, MemoryLayout<T>.stride)!.assumingMemoryBound(to: T.self)
    }

    func testTypedCameraZeroAndNestedAppResults() throws {
        let cameras = allocate(WDCamerasResult.self)
        cameras.pointee.items = allocate(WDCamera.self)
        cameras.pointee.count = 1
        cameras.pointee.items.pointee.name = strdup("Camera zero")
        let values = try GoClient.readCameras(cameras) // Consumes and frees C result.
        XCTAssertEqual(values.first?.id, 0)
        XCTAssertEqual(values.first?.name, "Camera zero")

        let apps = allocate(WDAppsResult.self)
        apps.pointee.count = 1
        apps.pointee.items = allocate(WDApp.self)
        apps.pointee.items.pointee.name = strdup("fixture")
        apps.pointee.items.pointee.state = WD_APP_CRASH_LOOPING
        apps.pointee.items.pointee.http_port = 8080
        apps.pointee.items.pointee.services = allocate(WDService.self)
        apps.pointee.items.pointee.service_count = 1
        apps.pointee.items.pointee.services.pointee.name = strdup("api")
        apps.pointee.items.pointee.services.pointee.state = WD_APP_RUNNING
        let result = try GoClient.readApps(apps)
        XCTAssertEqual(result.first?.runningState, .crashLooping)
        XCTAssertEqual(result.first?.httpPort, 8080)
        XCTAssertEqual(result.first?.hasWebpage, true)
        XCTAssertEqual(result.first?.services.first?.state, .running)
        XCTAssertEqual(result.first?.services.first?.name, "api")
    }

    func testTypedErrorPreservesRecoveryFlag() {
        let message = strdup("identity changed")
        defer { free(message) }
        XCTAssertThrowsError(try GoClient.check(WDStatus(message: message, can_forget_identity: 1))) {
            XCTAssertTrue(($0 as? WendyError)?.canForgetIdentity == true)
        }
    }

    func testErrorPresentationPreservesCauseWithoutOverwhelmingTheScreen() {
        let cause = "Device is pinned; no route to host. " + String(repeating: "Diagnostic detail. ", count: 100)
        let failure = PresentedFailure(cause)
        XCTAssertLessThan(failure.message.count, 130)
        XCTAssertEqual(failure.details, cause)
        XCTAssertFalse(failure.message.contains("pinned"))
    }
    func testChangedIdentityHasActionableExplanation() {
        let failure = PresentedFailure("Connection blocked: device ci-iq8.local identity changed.")
        XCTAssertTrue(failure.message.contains("forget its saved identity"))
        XCTAssertTrue(failure.details.contains("ci-iq8.local"))
    }
    func testDiagnosticTokensAreRedacted() {
        let failure = PresentedFailure("Bearer example-test-token access_token=example-test-value")
        XCTAssertFalse(failure.details.contains("example-test-token"))
        XCTAssertFalse(failure.details.contains("example-test-value"))
    }
    func testRawVideoAndStructuredLogSurviveFreeingCResult() throws {
        let batch = allocate(WDEvents.self)
        batch.pointee.count = 2
        batch.pointee.items = allocate(WDEvent.self, count: 2)
        let video = batch.pointee.items!
        video.pointee.kind = WD_EVENT_VIDEO
        video.pointee.session = 2
        video.pointee.stream = 12
        video.pointee.byte_count = 4
        video.pointee.bytes = allocate(UInt8.self, count: 4)
        for (i, value) in [UInt8(0), 255, 1, 128].enumerated() { video.pointee.bytes[i] = value }
        let log = batch.pointee.items.advanced(by: 1)
        log.pointee.kind = WD_EVENT_LOG
        log.pointee.log.timestamp_ns = 1234567890123456789
        log.pointee.log.level = WD_LOG_ERROR
        log.pointee.log.body = strdup("first line\n  stack frame")
        log.pointee.log.app = strdup("fixture")
        let events = try GoClient.readEvents(batch)
        XCTAssertEqual(events[0].session, 2)
        XCTAssertEqual(events[0].stream, 12)
        guard case .video(let bytes) = events[0].payload else { return XCTFail("Expected video") }
        XCTAssertEqual(bytes, Data([0, 255, 1, 128]))
        guard case .log(let record) = events[1].payload else { return XCTFail("Expected log") }
        XCTAssertEqual(record.timestampNanoseconds, 1234567890123456789)
        XCTAssertEqual(record.level, .error)
        XCTAssertEqual(record.message, "first line\n  stack frame")
        XCTAssertEqual(record.app, "fixture")
    }

    func testEmptyEventBatchAndTerminalFormatting() throws {
        XCTAssertTrue(try GoClient.readEvents(allocate(WDEvents.self)).isEmpty)
        let entry = LogEntry(timestampNanoseconds: 0, level: .warning,
                             body: "\u{001B}[31mWarning\u{001B}[0m\n  detail\n", app: "", service: "")
        XCTAssertEqual(entry.message, "Warning\n  detail")
        XCTAssertEqual(entry.time, "—")
    }

    func testVPNConfigurationPropertyListRoundTrip() throws {
        let config = VPNConfiguration(endpoint: "127.0.0.1:1234", token: "test-only",
                                      routes: [VPNRoute(selector: "mesh:fixture", name: "Fixture", address: "10.99.1.2", host: "device-fixture.mesh.wendy.dev")])
        let copy = try PropertyListDecoder().decode(VPNConfiguration.self, from: PropertyListEncoder().encode(config))
        copy.withCValue {
            XCTAssertEqual(String(cString: $0.pointee.endpoint), config.endpoint)
            XCTAssertEqual($0.pointee.route_count, 1)
            XCTAssertEqual(String(cString: $0.pointee.routes.pointee.host), config.routes[0].host)
        }
    }
    func testResourceSnapshotCopiesOptionalAndNestedReadings() {
        let result = allocate(WDResourcesResult.self)
        result.pointee.has_host = 1
        result.pointee.cpu = WDOptionalDouble(available: 1, value: 25)
        result.pointee.cpu_count = 4
        result.pointee.gpu_count = 1
        result.pointee.gpus = allocate(WDGPU.self)
        result.pointee.gpus.pointee.name = strdup("GPU fixture")
        result.pointee.gpus.pointee.temperature = WDOptionalDouble(available: 1, value: 0)
        result.pointee.row_count = 1
        result.pointee.rows = allocate(WDResourceRow.self)
        result.pointee.rows.pointee.id = strdup("app/api")
        result.pointee.rows.pointee.app = strdup("app")
        result.pointee.rows.pointee.name = strdup("api")
        result.pointee.rows.pointee.is_service = 1
        result.pointee.rows.pointee.state = WD_APP_CRASH_LOOPING
        let copy = ResourceSnapshot(result.pointee)
        WDResourcesFree(result)
        XCTAssertEqual(copy.cpu, 25)
        XCTAssertEqual(copy.gpus.first?.name, "GPU fixture")
        XCTAssertEqual(copy.gpus.first?.temperature, 0)
        XCTAssertNil(copy.gpus.first?.power)
        XCTAssertNil(copy.battery)
        XCTAssertEqual(copy.rows.first?.id, "app/api")
        XCTAssertEqual(copy.rows.first?.state, .crashLooping)
        XCTAssertNil(copy.rows.first?.cpu)
    }

    func testOrganizationSnapshotCopiesRulesAndRolesBeforeFree() {
        let result = allocate(WDOrganizationResult.self)
        result.pointee.name = strdup("Example")
        result.pointee.member_count = 1
        result.pointee.members = allocate(WDCloudMember.self)
        result.pointee.members.pointee.email = strdup("person@example.com")
        result.pointee.members.pointee.id = strdup("person")
        result.pointee.access_count = 1
        result.pointee.access = allocate(WDAccessEntry.self)
        result.pointee.access.pointee.id = strdup("rule")
        result.pointee.access.pointee.principal_type = WD_PRINCIPAL_GROUP
        result.pointee.access.pointee.principal_name = strdup("Operators")
        result.pointee.access.pointee.target_type = WD_TARGET_ALL
        result.pointee.access.pointee.effect = WD_ACCESS_DENY
        result.pointee.permissions_error = strdup("Permission lookup unavailable")
        let copy = CloudManagementClient.snapshot(result.pointee)
        WDOrganizationFree(result)
        XCTAssertEqual(copy.name, "Example")
        XCTAssertEqual(copy.members.first?.title, "person@example.com")
        XCTAssertEqual(copy.rules.first?.principalKind, "Group")
        XCTAssertEqual(copy.rules.first?.target, "All devices")
        XCTAssertEqual(copy.rules.first?.effectName, "Deny")
        XCTAssertTrue(copy.permissions.isEmpty)
        XCTAssertEqual(copy.permissionsError, "Permission lookup unavailable")
    }

}
