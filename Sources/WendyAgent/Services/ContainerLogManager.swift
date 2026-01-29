import ContainerdGRPC
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import OpenTelemetryGRPC

struct LogChunk: Sendable {
    let data: Data
    let isStderr: Bool
}

actor ContainerLogManager {
    static let shared = ContainerLogManager()

    private struct StartError: Error, Sendable {
        let message: String
    }

    private let logger = Logger(label: "ContainerLogManager")
    private var logTasks: [String: Task<Void, Never>] = [:]
    private var subscribers: [String: [UUID: AsyncStream<LogChunk>.Continuation]] = [:]

    func startContainer(
        appName: String,
        markExplicitStop: Bool
    ) async throws -> AsyncStream<LogChunk> {
        let (id, stream) = subscribe(appName: appName)
        do {
            try await startLogging(appName: appName, markExplicitStop: markExplicitStop)
            return stream
        } catch {
            unsubscribe(appName: appName, id: id)
            throw error
        }
    }

    func restartContainer(appName: String) async throws {
        try await startLogging(appName: appName, markExplicitStop: false)
    }

    private func subscribe(appName: String) -> (UUID, AsyncStream<LogChunk>) {
        let id = UUID()
        let (stream, continuation) = AsyncStream<LogChunk>.makeStream(
            bufferingPolicy: .bufferingNewest(200)
        )

        var appSubscribers = subscribers[appName, default: [:]]
        appSubscribers[id] = continuation
        subscribers[appName] = appSubscribers

        continuation.onTermination = { [weak self] _ in
            Task { await self?.unsubscribe(appName: appName, id: id) }
        }

        return (id, stream)
    }

    private func unsubscribe(appName: String, id: UUID) {
        guard var appSubscribers = subscribers[appName] else {
            return
        }

        if let continuation = appSubscribers.removeValue(forKey: id) {
            continuation.finish()
        }

        if appSubscribers.isEmpty {
            subscribers.removeValue(forKey: appName)
        } else {
            subscribers[appName] = appSubscribers
        }
    }

    private func startLogging(
        appName: String,
        markExplicitStop: Bool
    ) async throws {
        logTasks[appName]?.cancel()

        try await withCheckedThrowingContinuation { continuation in
            let task = Task.detached(priority: .userInitiated) { [appName] in
                let taskLogger = Logger(label: "ContainerLogManager.log-task")
                var startResumed = false

                func resumeOnce(_ result: Result<Void, Error>) {
                    guard !startResumed else { return }
                    startResumed = true
                    continuation.resume(with: result)
                }

                defer {
                    if !startResumed {
                        resumeOnce(
                            .failure(StartError(message: "Container start canceled"))
                        )
                    }
                }

                do {
                    try await Containerd.withClient { client in
                        _ = try await client.withStdout { stdout, stderr in
                            try await self.startTask(
                                client: client,
                                appName: appName,
                                stdout: stdout,
                                stderr: stderr,
                                markExplicitStop: markExplicitStop,
                                logger: taskLogger
                            )
                            resumeOnce(.success(()))
                        } onStdout: { bytes in
                            await self.handleOutput(
                                appName: appName,
                                bytes: bytes,
                                isStderr: false
                            )
                        } onStderr: { bytes in
                            await self.handleOutput(
                                appName: appName,
                                bytes: bytes,
                                isStderr: true
                            )
                        }
                    }
                } catch is CancellationError {
                    // Expected on cancellation.
                } catch {
                    resumeOnce(.failure(error))
                    taskLogger.error(
                        "Failed to run container log task",
                        metadata: [
                            "container-id": .stringConvertible(appName),
                            "error": .stringConvertible("\(error)"),
                        ]
                    )
                }
            }

            logTasks[appName] = task
        }
    }

    private func startTask(
        client: Containerd,
        appName: String,
        stdout: String,
        stderr: String,
        markExplicitStop: Bool,
        logger: Logger
    ) async throws {
        let container = try await client.getContainer(named: appName)
        let snapshot = try await client.mountsSnapshot(named: container.snapshotKey)

        try await stopExistingTask(
            client: client,
            appName: appName,
            markExplicitStop: markExplicitStop,
            logger: logger
        )

        do {
            logger.info("Creating task")
            try await client.createTask(
                containerID: appName,
                appName: appName,
                mounts: snapshot.mounts,
                stdout: stdout,
                stderr: stderr,
                runtime: container.runtime.name
            )
        } catch let error as RPCError where error.code == .alreadyExists {
            logger.info(
                "Task already exists, re-creating it",
                metadata: [
                    "container-id": .stringConvertible(appName)
                ]
            )
            try await client.deleteTask(containerID: appName)
            logger.debug(
                "Task removed, recreating",
                metadata: [
                    "container-id": .stringConvertible(appName)
                ]
            )
            try await client.createTask(
                containerID: appName,
                appName: appName,
                mounts: snapshot.mounts,
                stdout: stdout,
                stderr: stderr,
                runtime: container.runtime.name
            )
        } catch is RPCError {
            logger.error(
                "Failed to kill container",
                metadata: [
                    "container-id": .stringConvertible(appName)
                ]
            )
            try await client.createTask(
                containerID: appName,
                appName: appName,
                mounts: snapshot.mounts,
                stdout: stdout,
                stderr: stderr,
                runtime: container.runtime.name
            )
            logger.debug(
                "Task created",
                metadata: [
                    "container-id": .stringConvertible(appName)
                ]
            )
        }

        logger.info("Starting task")
        try await client.runTask(containerID: appName)
        await ContainerMonitor.shared.markContainerStarted(appName)
    }

    private func stopExistingTask(
        client: Containerd,
        appName: String,
        markExplicitStop: Bool,
        logger: Logger
    ) async throws {
        do {
            try await client.stopTask(containerID: appName)
            if markExplicitStop {
                await ContainerMonitor.shared.markContainerStopped(appName)
            }
            logger.info(
                "Stopped container before restart",
                metadata: ["container-id": .stringConvertible(appName)]
            )
        } catch let error as RPCError where error.code == .notFound {
            logger.info(
                "Container wasn't running",
                metadata: ["container-id": .stringConvertible(appName)]
            )
        } catch let error as RPCError {
            logger.error(
                "Failed to stop container",
                metadata: [
                    "container-id": .stringConvertible(appName),
                    "error": .stringConvertible(error.description),
                ]
            )
            throw error
        }
    }

    private func handleOutput(
        appName: String,
        bytes: ByteBuffer,
        isStderr: Bool
    ) async {
        let data = Data(buffer: bytes)
        let chunk = LogChunk(data: data, isStderr: isStderr)
        broadcast(chunk, appName: appName)

        if let broadcaster = TelemetryLogBroadcasterHolder.shared.broadcaster {
            let output = String(buffer: bytes)
            let logRequest = createContainerLogRequest(
                appName: appName,
                output: output,
                isStderr: isStderr
            )
            await broadcaster.broadcastLogs(logRequest)
        }
    }

    private func broadcast(_ chunk: LogChunk, appName: String) {
        guard var appSubscribers = subscribers[appName] else {
            return
        }

        for (id, continuation) in appSubscribers {
            let result = continuation.yield(chunk)
            if case .terminated = result {
                appSubscribers.removeValue(forKey: id)
            }
        }

        if appSubscribers.isEmpty {
            subscribers.removeValue(forKey: appName)
        } else {
            subscribers[appName] = appSubscribers
        }
    }
}

private func createContainerLogRequest(
    appName: String,
    output: String,
    isStderr: Bool
) -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest {
    let timestamp = UInt64(Date().timeIntervalSince1970 * 1_000_000_000)

    var logRecord = Opentelemetry_Proto_Logs_V1_LogRecord()
    logRecord.timeUnixNano = timestamp
    logRecord.observedTimeUnixNano = timestamp
    logRecord.severityNumber = isStderr ? .warn : .info
    logRecord.severityText = isStderr ? "STDERR" : "STDOUT"
    logRecord.body = .with { $0.stringValue = output }

    // Add stream type as attribute
    var streamAttr = Opentelemetry_Proto_Common_V1_KeyValue()
    streamAttr.key = "stream"
    streamAttr.value = .with { $0.stringValue = isStderr ? "stderr" : "stdout" }
    logRecord.attributes.append(streamAttr)

    var scopeLogs = Opentelemetry_Proto_Logs_V1_ScopeLogs()
    scopeLogs.logRecords = [logRecord]

    var resourceLogs = Opentelemetry_Proto_Logs_V1_ResourceLogs()
    resourceLogs.scopeLogs = [scopeLogs]

    // Add service name attribute (the app name)
    var serviceNameAttr = Opentelemetry_Proto_Common_V1_KeyValue()
    serviceNameAttr.key = "service.name"
    serviceNameAttr.value = .with { $0.stringValue = appName }
    resourceLogs.resource.attributes.append(serviceNameAttr)

    // Add wendy.app.name for filtering
    var appNameAttr = Opentelemetry_Proto_Common_V1_KeyValue()
    appNameAttr.key = "wendy.app.name"
    appNameAttr.value = .with { $0.stringValue = appName }
    resourceLogs.resource.attributes.append(appNameAttr)

    return Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest.with {
        $0.resourceLogs = [resourceLogs]
    }
}
