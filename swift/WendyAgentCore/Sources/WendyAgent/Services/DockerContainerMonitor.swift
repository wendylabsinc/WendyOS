import Foundation
import Logging
import Subprocess

struct DockerContainerEvent: Sendable, Equatable {
    enum Kind: Sendable, Equatable {
        case started
        case stopped
        case removed
    }

    let appName: String
    let kind: Kind
}

actor DockerContainerMonitor {
    private enum MonitorError: Error, CustomStringConvertible {
        case streamExited(status: Int32)
        case streamSignaled(signal: Int32)

        var description: String {
            switch self {
            case .streamExited(let status):
                "docker events exited with status \(status)"
            case .streamSignaled(let signal):
                "docker events terminated by signal \(signal)"
            }
        }
    }

    private let dockerExecutable: String
    private let logger: Logger
    private let onEvent: @Sendable (DockerContainerEvent) async -> Void
    private let onStreamInterrupted: @Sendable () async -> Void
    private let restartDelay: Duration = .seconds(1)
    private var monitorTask: Task<Void, Never>?

    init(
        dockerExecutable: String,
        logger: Logger,
        onEvent: @escaping @Sendable (DockerContainerEvent) async -> Void,
        onStreamInterrupted: @escaping @Sendable () async -> Void
    ) {
        self.dockerExecutable = dockerExecutable
        self.logger = logger
        self.onEvent = onEvent
        self.onStreamInterrupted = onStreamInterrupted
    }

    func start() {
        guard self.monitorTask == nil else { return }

        let monitor = self
        self.monitorTask = Task {
            await monitor.run()
        }
    }

    func stop() async {
        guard let monitorTask = self.monitorTask else { return }
        self.monitorTask = nil
        monitorTask.cancel()
        await monitorTask.value
    }

    private func run() async {
        while !Task.isCancelled {
            do {
                try await self.streamEvents()
            } catch is CancellationError {
                break
            } catch {
                self.logger.warning(
                    "Docker container monitor interrupted",
                    metadata: ["error": "\(String(describing: error))"]
                )
                await self.onStreamInterrupted()
            }

            guard !Task.isCancelled else { break }
            try? await Task.sleep(for: self.restartDelay)
        }
    }

    private func streamEvents() async throws {
        self.logger.info("Starting Docker container event monitor")

        let outcome = try await Subprocess.run(
            .name(self.dockerExecutable),
            arguments: Arguments([
                "events",
                "--filter", "label=wendy.managed=true",
                "--format", "{{.Status}}\t{{index .Actor.Attributes \"wendy.app-name\"}}",
            ])
        ) { execution, outputSequence in
            var pending = Data()

            do {
                for try await buffer in outputSequence {
                    pending.append(buffer.withUnsafeBytes { Data($0) })

                    while let newlineIndex = pending.firstIndex(of: 0x0A) {
                        let lineData = pending[..<newlineIndex]
                        pending.removeSubrange(...newlineIndex)
                        await self.handleEventLine(String(decoding: lineData, as: UTF8.self))
                    }
                }

                if !pending.isEmpty {
                    await self.handleEventLine(String(decoding: pending, as: UTF8.self))
                }
            } catch {
                await execution.teardown(
                    using: [.gracefulShutDown(allowedDurationToNextStep: .seconds(1))]
                )
                throw error
            }
        }

        switch outcome.terminationStatus {
        case .exited(0):
            throw MonitorError.streamExited(status: 0)
        case .exited(let status):
            throw MonitorError.streamExited(status: status)
        case .signaled(_) where Task.isCancelled:
            throw CancellationError()
        case .signaled(let signal):
            throw MonitorError.streamSignaled(signal: signal)
        }
    }

    private func handleEventLine(_ rawLine: String) async {
        let line = rawLine.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !line.isEmpty else { return }

        let parts = line.split(separator: "\t", maxSplits: 1).map(String.init)
        guard parts.count == 2 else {
            self.logger.debug(
                "Ignoring malformed Docker event line",
                metadata: ["line": "\(line)"]
            )
            return
        }

        let status = parts[0]
        let appName = parts[1]
        let event: DockerContainerEvent?

        switch status {
        case "start":
            event = DockerContainerEvent(appName: appName, kind: .started)
        case "die", "stop":
            event = DockerContainerEvent(appName: appName, kind: .stopped)
        case "destroy":
            event = DockerContainerEvent(appName: appName, kind: .removed)
        default:
            event = nil
        }

        guard let event else { return }

        self.logger.info(
            "Observed Docker container event",
            metadata: [
                "app_name": "\(event.appName)",
                "event": "\(String(describing: event.kind))",
            ]
        )
        await self.onEvent(event)
    }
}
