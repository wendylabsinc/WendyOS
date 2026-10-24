import XCTest
import Synchronization
#if os(iOS)
@testable import WendyMobile
#else
@testable import WendyDesktop
#endif

@MainActor
final class ConcurrencyTests: XCTestCase {
    func testCancelledQueuedWorkDoesNotCallTheBridge() async {
        let queue = DispatchQueue(label: "test.cancelled-bridge")
        queue.suspend()
        let called = Mutex(false)
        let started = AsyncCallback<Void>()
        await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                started.finish(.success(()))
                do {
                    _ = try await BlockingWork.run(on: queue) { called.withLock { $0 = true }; return 42 }
                    return false
                } catch { return error is CancellationError }
            }
            _ = try? await started.value()
            group.cancelAll()
            queue.resume()
            let cancelled = await group.next()
            XCTAssertEqual(cancelled, true)
        }
        XCTAssertFalse(called.withLock { $0 })
    }

    func testRunningBridgeFinishesBeforeCancellationReturns() async {
        let queue = DispatchQueue(label: "test.running-bridge")
        let release = DispatchSemaphore(value: 0)
        let started = AsyncCallback<Void>()
        let finished = Mutex(false)
        await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                do {
                    _ = try await BlockingWork.run(on: queue) {
                        started.finish(.success(()))
                        release.wait()
                        finished.withLock { $0 = true }
                        return 42
                    }
                    return false
                } catch { return error is CancellationError }
            }
            _ = try? await started.value()
            group.cancelAll()
            XCTAssertFalse(finished.withLock { $0 })
            release.signal()
            let cancelled = await group.next()
            XCTAssertEqual(cancelled, true)
        }
        XCTAssertTrue(finished.withLock { $0 })
    }

    func testCleanupRunsInCancelledTask() async throws {
        try await withThrowingTaskGroup(of: Int.self) { group in
            group.addTask {
                withUnsafeCurrentTask { $0?.cancel() }
                return try await BlockingWork.run(on: .global(), unlessCancelled: false) { 42 }
            }
            let result = try await group.next()
            XCTAssertEqual(result, 42)
        }
    }

    func testCallbackHandlesCompletionBeforeWaitAndDuplicateCompletion() async throws {
        let callback = AsyncCallback<Int>()
        callback.finish(.success(42))
        callback.finish(.success(100))
        let result = try await callback.value()
        XCTAssertEqual(result, 42)
    }

    func testCallbackHandlesCancellationBeforeWait() async {
        let callback = AsyncCallback<Int>()
        await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                withUnsafeCurrentTask { $0?.cancel() }
                do { _ = try await callback.value(); return false }
                catch { return error is CancellationError }
            }
            let cancelled = await group.next()
            XCTAssertEqual(cancelled, true)
        }
        callback.finish(.success(42))
    }

    func testCallbackCancellationUnblocksWaitingTask() async {
        let callback = AsyncCallback<Int>()
        let started = AsyncCallback<Void>()
        await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                started.finish(.success(()))
                do { _ = try await callback.value(); return false }
                catch { return error is CancellationError }
            }
            _ = try? await started.value()
            group.cancelAll()
            let cancelled = await group.next()
            XCTAssertEqual(cancelled, true)
        }
        callback.finish(.success(42))
    }

    func testActionScopeCancelsAndJoinsChildren() async {
        let actions = AsyncActions()
        let started = AsyncCallback<Void>()
        var cleanedUp = false
        actions.submit {
            defer { cleanedUp = true }
            started.finish(.success(()))
            try? await Task.sleep(for: .seconds(60))
        }
        await withTaskGroup(of: Void.self) { group in
            group.addTask { await actions.run() }
            _ = try? await started.value()
            group.cancelAll()
        }
        XCTAssertTrue(cleanedUp)
    }

    func testActionScopeDrainsAndCanRestart() async {
        let actions = AsyncActions()
        var completed = 0
        for _ in 0..<2 {
            let started = AsyncCallback<Void>()
            actions.submit {
                completed += 1
                started.finish(.success(()))
            }
            await withTaskGroup(of: Void.self) { group in
                group.addTask { await actions.run() }
                _ = try? await started.value()
                await actions.finishAndWait()
            }
        }
        XCTAssertEqual(completed, 2)
    }

    func testCleanupDrainsWithoutAnActiveView() async {
        let actions = AsyncActions()
        var completed = false
        actions.submit { completed = true }
        await actions.finishAndWait()
        XCTAssertTrue(completed)
    }
}
