import Darwin
import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("ContainerService.startContainer")
struct ContainerServiceTests {
    @Test("file-sync native launch uses synced app directory as current working directory")
    func fileSyncLaunchUsesSyncedAppDirectoryAsCurrentWorkingDirectory() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.PrintPWD"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)

        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "printpwd.sh")
        let stdout = try await startAppAndCollectStdout(service: service, appID: appID)
        let expectedPath = try canonicalPath(appDirectory.path)

        #expect(stdout == expectedPath)
    }

    @Test("sandboxed file-sync native launch uses synced app directory as current working directory")
    func sandboxedFileSyncLaunchUsesSyncedAppDirectoryAsCurrentWorkingDirectory() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.PrintPWDSandboxed"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)

        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))
        try writeSandboxProfile(to: appDirectory.appendingPathComponent("sandbox.sb"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "printpwd.sh")
        let stdout = try await startAppAndCollectStdout(service: service, appID: appID)
        let expectedPath = try canonicalPath(appDirectory.path)

        #expect(stdout == expectedPath)
    }

    @Test("listContainerStats reports registered apps with zeroed stats")
    func listContainerStatsReportsRegisteredApps() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.Stats"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "printpwd.sh")
        let stats = try await listContainerStats(service: service)

        #expect(stats.count == 1)
        #expect(stats.first?.appName == appID)
        #expect(stats.first?.memoryBytes == 0)
        #expect(stats.first?.storageBytes == 0)
    }
}

// MARK: - Helpers

private final class CollectingWriter<Element: Sendable>: RPCWriterProtocol, @unchecked Sendable {
    private let queue = DispatchQueue(label: "wendy.tests.collecting-writer")
    private var elements: [Element] = []

    func write(_ element: Element) async throws {
        queue.sync {
            elements.append(element)
        }
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        queue.sync {
            self.elements.append(contentsOf: elements)
        }
    }

    func snapshot() -> [Element] {
        queue.sync {
            elements
        }
    }
}

private struct TestError: Error, CustomStringConvertible {
    let description: String
}

private func registerFileSyncApp(
    service: ContainerService,
    appID: String,
    cmd: String
) async throws {
    var request = Wendy_Agent_Services_V1_CreateContainerRequest()
    request.appName = appID
    request.imageName = ""
    request.cmd = cmd

    _ = try await service.createContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "CreateContainer")
    )
}

private func startAppAndCollectStdout(
    service: ContainerService,
    appID: String
) async throws -> String {
    var request = Wendy_Agent_Services_V1_StartContainerRequest()
    request.appName = appID

    let response = try await service.startContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StartContainer")
    )

    let contents = try response.accepted.get()
    let writer = CollectingWriter<Wendy_Agent_Services_V1_RunContainerLayersResponse>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))

    let messages = writer.snapshot()
    let stdout = messages.reduce(into: Data()) { data, message in
        guard case .stdoutOutput(let output)? = message.responseType else { return }
        data.append(output.data)
    }
    let stderr = messages.reduce(into: Data()) { data, message in
        guard case .stderrOutput(let output)? = message.responseType else { return }
        data.append(output.data)
    }

    let stdoutText = String(decoding: stdout, as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)
    let stderrText = String(decoding: stderr, as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)

    if stdoutText.isEmpty, !stderrText.isEmpty {
        throw TestError(description: "Process produced no stdout. stderr: \(stderrText)")
    }

    return stdoutText
}

private func listContainerStats(
    service: ContainerService
) async throws -> [Wendy_Agent_Services_V1_ContainerStats] {
    let response = try await service.listContainerStats(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V1_ListContainerStatsRequest()
        ),
        context: makeServerContext(method: "ListContainerStats")
    )
    return try response.message.stats
}

private func makeServerContext(method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyContainerService",
            method: method
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}

private func writePrintPWDScript(to url: URL) throws {
    try "#!/bin/sh\n/bin/pwd\n".write(to: url, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func writeSandboxProfile(to url: URL) throws {
    try "(version 1)\n(allow default)\n".write(to: url, atomically: true, encoding: .utf8)
}

private func makeTempDir() throws -> String {
    let path =
        FileManager.default.temporaryDirectory
        .appendingPathComponent("wendy-test-\(UUID().uuidString)").path
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
    return path
}

private func canonicalPath(_ path: String) throws -> String {
    var resolved = [CChar](repeating: 0, count: Int(PATH_MAX))
    guard path.withCString({ realpath($0, &resolved) }) != nil else {
        throw TestError(description: "Failed to resolve canonical path for \(path)")
    }
    let count = resolved.firstIndex(of: 0) ?? resolved.count
    return String(decoding: resolved.prefix(count).map(UInt8.init(bitPattern:)), as: UTF8.self)
}

private func cleanup(_ path: String) {
    try? FileManager.default.removeItem(atPath: path)
}
