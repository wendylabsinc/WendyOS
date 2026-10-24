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
        let started = AsyncStream<Void>.makeStream()
        let task = Task {
            started.continuation.yield(())
            return try await BlockingWork.run(on: queue) { called.withLock { $0 = true }; return 42 }
        }
        for await _ in started.stream { break }
        task.cancel()
        queue.resume()
        do { _ = try await task.value; XCTFail("Cancelled work returned a value") }
        catch { XCTAssertTrue(error is CancellationError) }
        XCTAssertFalse(called.withLock { $0 })
    }

    func testRunningBridgeFinishesBeforeCancellationReturns() async {
        let queue = DispatchQueue(label: "test.running-bridge")
        let release = DispatchSemaphore(value: 0)
        let started = AsyncStream<Void>.makeStream()
        let finished = Mutex(false)
        let task = Task {
            try await BlockingWork.run(on: queue) {
                started.continuation.yield(())
                release.wait()
                finished.withLock { $0 = true }
                return 42
            }
        }
        for await _ in started.stream { break }
        task.cancel()
        XCTAssertFalse(finished.withLock { $0 })
        release.signal()
        do { _ = try await task.value; XCTFail("Cancelled work returned a value") }
        catch { XCTAssertTrue(error is CancellationError) }
        XCTAssertTrue(finished.withLock { $0 })
    }

    func testCleanupRunsInCancelledTask() async throws {
        let task = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            return try await BlockingWork.run(on: .global(), unlessCancelled: false) { 42 }
        }
        let result = try await task.value
        XCTAssertEqual(result, 42)
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
        let task = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            return try await callback.value()
        }
        do { _ = try await task.value; XCTFail("Cancelled callback returned a value") }
        catch { XCTAssertTrue(error is CancellationError) }
        callback.finish(.success(42)) // A late delegate callback must not resume twice.
    }

    func testCallbackCancellationUnblocksWaitingTask() async {
        let callback = AsyncCallback<Int>()
        let started = AsyncStream<Void>.makeStream()
        let task = Task {
            started.continuation.yield(())
            return try await callback.value()
        }
        for await _ in started.stream { break }
        task.cancel()
        do { _ = try await task.value; XCTFail("Cancelled callback returned a value") }
        catch { XCTAssertTrue(error is CancellationError) }
        callback.finish(.success(42))
    }
}
