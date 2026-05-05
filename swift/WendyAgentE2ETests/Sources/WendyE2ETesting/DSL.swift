import Subprocess

public struct MachineCommand: Sendable {
    public enum PollCondition: Sendable {
        case success
        case failure
    }

    public let machine: Machine
    public let command: String

    public func poll(
        until condition: PollCondition,
        step: Duration = .milliseconds(250),
        timeout: Duration = .seconds(10),
        timeoutMessage: String? = nil
    ) -> MachineCommand {
        precondition(step > .zero, "step must be greater than zero")
        precondition(timeout >= .zero, "timeout must be greater than or equal to zero")

        return MachineCommand(
            machine: self.machine,
            command: self.command,
            pollConfiguration: PollConfiguration(
                condition: condition,
                step: step,
                timeout: timeout,
                timeoutMessage: timeoutMessage
            )
        )
    }

    public func run(
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        guard let pollConfiguration else {
            try await self.machine.run(
                self.command,
                filePath: filePath,
                function: function,
                line: line
            )
            return
        }

        try await self.poll(
            pollConfiguration,
            filePath: filePath,
            function: function,
            line: line
        )
    }

    // MARK: - Internal

    init(machine: Machine, command: String) {
        self.machine = machine
        self.command = command
        self.pollConfiguration = nil
    }

    // MARK: - Private

    private let pollConfiguration: PollConfiguration?

    private init(
        machine: Machine,
        command: String,
        pollConfiguration: PollConfiguration?
    ) {
        self.machine = machine
        self.command = command
        self.pollConfiguration = pollConfiguration
    }

    private func poll(
        _ configuration: PollConfiguration,
        filePath: String,
        function: String,
        line: Int
    ) async throws {
        let clock = ContinuousClock()
        let start = clock.now
        var lastTerminationStatus: TerminationStatus?

        while true {
            let record = try await self.machine.run(
                self.command,
                output: .string(limit: .max),
                error: .string(limit: .max),
                filePath: filePath,
                function: function,
                line: line
            )
            lastTerminationStatus = record.terminationStatus

            if configuration.condition.matches(record.terminationStatus) {
                return
            }

            let elapsed = start.duration(to: clock.now)
            guard elapsed < configuration.timeout else {
                throw MachineError.pollTimedOut(
                    machine: self.machine.description,
                    command: self.command,
                    condition: configuration.condition.description,
                    timeout: configuration.timeout,
                    lastTerminationStatus: lastTerminationStatus,
                    message: configuration.timeoutMessage
                )
            }

            try await clock.sleep(for: min(configuration.step, configuration.timeout - elapsed))
        }
    }
}

// MARK: - CustomStringConvertible

extension MachineCommand.PollCondition: CustomStringConvertible {
    public var description: String {
        switch self {
        case .success:
            return "success"
        case .failure:
            return "failure"
        }
    }
}

// MARK: - Private

private struct PollConfiguration: Sendable {
    let condition: MachineCommand.PollCondition
    let step: Duration
    let timeout: Duration
    let timeoutMessage: String?
}

extension MachineCommand.PollCondition {
    fileprivate func matches(_ terminationStatus: TerminationStatus) -> Bool {
        switch self {
        case .success:
            return terminationStatus.isSuccess
        case .failure:
            return !terminationStatus.isSuccess
        }
    }
}
