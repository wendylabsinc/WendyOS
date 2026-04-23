import Dispatch

private enum BonjourDNSExecutionContext {
    static let queue = DispatchQueue(label: "sh.wendy.agent.bonjour.registration")
    static let executor = DispatchQueueSerialExecutor(queue: queue)
}

@globalActor
actor BonjourDNSActor {
    static let shared = BonjourDNSActor()

    nonisolated static var dispatchQueue: DispatchQueue {
        BonjourDNSExecutionContext.queue
    }

    nonisolated var unownedExecutor: UnownedSerialExecutor {
        BonjourDNSExecutionContext.executor.asUnownedSerialExecutor()
    }

    private init() {}
}

final class DispatchQueueSerialExecutor: SerialExecutor {
    private let queue: DispatchQueue

    init(queue: DispatchQueue) {
        self.queue = queue
    }

    func enqueue(_ job: consuming ExecutorJob) {
        let unownedJob = UnownedJob(job)
        let executor = self.asUnownedSerialExecutor()

        self.queue.async {
            unownedJob.runSynchronously(on: executor)
        }
    }
}
