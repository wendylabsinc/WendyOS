import XCTest
@testable import WorldOperator

final class WorkspaceTests: XCTestCase {
    func testExplicitOperatorProfileArguments() throws {
        XCTAssertNil(try WorldOperatorLaunchRequest.parse(["Wendy"]))
        XCTAssertEqual(try WorldOperatorLaunchRequest.parse(["--world-profile", "/tmp/explicit-profile.json"])?.path, "/tmp/explicit-profile.json")
        XCTAssertThrowsError(try WorldOperatorLaunchRequest.parse(["--profile", "relative.json"]))
        XCTAssertThrowsError(try WorldOperatorLaunchRequest.parse(["--profile", "/tmp/a", "--world-profile", "/tmp/b"]))
    }

    @MainActor
    func testChatRoutesOnlyChangeWorkspacePage() {
        let session = WorldOperatorSession(storageDirectory: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString))
        for section in WorldWorkspaceSection.allCases {
            session.open(section)
            XCTAssertEqual(session.section, section)
            XCTAssertFalse(session.model.canMission)
            XCTAssertFalse(session.model.canAnnotate)
            XCTAssertNil(session.model.snapshot)
        }
        session.open(.missions); XCTAssertEqual(session.page, .missions)
        session.open(.annotations); XCTAssertEqual(session.page, .notes)
        session.open(.areas); XCTAssertEqual(session.page, .areas)
        session.open(.operations); XCTAssertEqual(session.page, .world)
    }

    @MainActor
    func testNewWorkspaceHasNoImplicitAuthority() {
        let model = OperatorModel(storageDirectory: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString))
        XCTAssertFalse(model.canMission)
        XCTAssertFalse(model.canAnnotate)
        XCTAssertTrue(model.offline)
        XCTAssertNil(model.snapshot)
    }

    @MainActor
    func testHiddenWorkspaceEndsActiveParticipation() async {
        let model = OperatorModel(storageDirectory: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString))
        await withTaskGroup(of: Void.self) { group in
            defer { group.cancelAll() }
            group.addTask { await model.run() }
            await Task.yield()
        }
        XCTAssertFalse(model.sceneActive)
    }

    @MainActor
    func testActionsFinishWhenWorkspaceIsCancelled() async {
        let actions = WorldActions()
        let started = expectation(description: "action started")
        let stopped = expectation(description: "action cancelled")
        actions.submit {
            started.fulfill()
            do { try await Task.sleep(for: .seconds(60)) }
            catch { stopped.fulfill() }
        }
        await withTaskGroup(of: Void.self) { group in
            defer { group.cancelAll() }
            group.addTask { await actions.run() }
            await fulfillment(of: [started], timeout: 2)
        }
        await fulfillment(of: [stopped], timeout: 2)
    }
}
