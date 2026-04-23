import Dispatch

private enum BonjourRegistrationExecutionContext {
    static let queue = DispatchQueue(label: "sh.wendy.agent.bonjour.registration")
    static let executor = DispatchQueueSerialExecutor(queue: queue)
}

@globalActor
actor BonjourRegistrationActor {
    static let shared = BonjourRegistrationActor()

    nonisolated static var dispatchQueue: DispatchQueue {
        BonjourRegistrationExecutionContext.queue
    }

    nonisolated var unownedExecutor: UnownedSerialExecutor {
        BonjourRegistrationExecutionContext.executor.asUnownedSerialExecutor()
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
