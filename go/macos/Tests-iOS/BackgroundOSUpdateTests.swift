import XCTest
@testable import WendyMobile

@MainActor
private final class FakeOSBackgroundTask: OSUpdateBackgroundTask {
    var expire: (@MainActor @Sendable () -> Void)?
    let completed = AsyncCallback<Void>()
    var completions: [Bool] = []
    var percentages: [Int] = []
    func onExpiration(_ action: @escaping @MainActor @Sendable () -> Void) { expire = action }
    func report(_ install: OSUpdateSnapshot.Install) { percentages.append(install.percent) }
    func finish(success: Bool) { completions.append(success); completed.finish(.success(())) }
}

@MainActor
final class BackgroundOSUpdateTests: XCTestCase {
    func testRejectedRuntimeNeverStartsInstallation() throws {
        var starts = 0
        let runner = MobileOSUpdateTask(submit: { _, _ in throw CancellationError() }, update: { _, _, _ in
            starts += 1; throw CancellationError()
        }, cancelUpdate: { _ in XCTFail("No installation to cancel") })
        XCTAssertThrowsError(try runner.start(session: 7, scope: "a", name: "test", artifact: "https://example.test/os"))
        XCTAssertFalse(runner.active)
        XCTAssertEqual(starts, 0)
    }

    func testRuntimeGrantOwnsInstallAndReportsCompletion() async throws {
        var launch: MobileOSUpdateTask.Launch?
        var starts = 0
        let runner = MobileOSUpdateTask(submit: { _, handler in launch = handler }, update: { session, artifact, start in
            XCTAssertEqual(session, 7); XCTAssertTrue(start); XCTAssertFalse(artifact.isEmpty)
            starts += 1
            return try JSONDecoder().decode(OSUpdateSnapshot.self, from: Data(#"{"status":{},"install":{"active":false,"phase":"Installed","percent":100,"rebootRequired":true,"error":"","baseline":12}}"#.utf8))
        }, cancelUpdate: { _ in XCTFail("Successful install must not be cancelled") })
        try runner.start(session: 7, scope: "a", name: "test", artifact: "https://example.test/os")
        XCTAssertEqual(starts, 0, "Must wait for runtime grant")
        XCTAssertTrue(runner.keepsConnection(session: 7, scope: "a"))
        XCTAssertFalse(runner.keepsConnection(session: 8, scope: "a"))
        XCTAssertFalse(runner.keepsConnection(session: 7, scope: "b"))
        let task = FakeOSBackgroundTask()
        launch?(task)
        await withTaskGroup(of: Void.self) { group in
            group.addTask { await runner.run() }
            _ = try? await task.completed.value()
            group.cancelAll()
        }
        XCTAssertEqual(starts, 1)
        XCTAssertEqual(task.percentages, [100])
        XCTAssertEqual(task.completions, [true])
        XCTAssertTrue(runner.snapshot?.install.rebootRequired == true)
    }

    func testExpirationCancelsInstallationWithoutReportingSuccess() async throws {
        var launch: MobileOSUpdateTask.Launch?
        let started = AsyncCallback<Void>()
        let pending = AsyncCallback<OSUpdateSnapshot>()
        var cancelled: [UInt64] = []
        let runner = MobileOSUpdateTask(submit: { _, handler in launch = handler }, update: { _, _, _ in
            started.finish(.success(())); return try await pending.value()
        }, cancelUpdate: { session in
            cancelled.append(session); pending.finish(.failure(CancellationError()))
        })
        try runner.start(session: 7, scope: "a", name: "test", artifact: "https://example.test/os")
        let task = FakeOSBackgroundTask(); launch?(task)
        await withTaskGroup(of: Void.self) { group in
            group.addTask { await runner.run() }
            _ = try? await started.value()
            task.expire?()
            XCTAssertFalse(runner.keepsConnection(session: 7, scope: "a"))
            group.cancelAll()
        }
        XCTAssertTrue(cancelled.contains(7))
        XCTAssertEqual(task.completions, [false])
        XCTAssertNotNil(runner.error)
    }

    func testSelectionChangeRejectsLateGrant() throws {
        var launch: MobileOSUpdateTask.Launch?
        var cancelled: [UInt64] = []
        let runner = MobileOSUpdateTask(submit: { _, handler in launch = handler }, cancelUpdate: { cancelled.append($0) })
        try runner.start(session: 7, scope: "a", name: "test", artifact: "https://example.test/os")
        runner.selectionChanged(session: 7, scope: "b")
        let task = FakeOSBackgroundTask(); launch?(task)
        XCTAssertFalse(runner.active)
        XCTAssertEqual(cancelled, [7])
        XCTAssertEqual(task.completions, [false])
    }
}
