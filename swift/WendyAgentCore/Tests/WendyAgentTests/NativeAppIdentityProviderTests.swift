import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("Native app identity provider")
struct NativeAppIdentityProviderTests {
    @Test
    func `identity socket paths are absolute normalized Unix socket paths`() throws {
        let lease = try NativeAppIdentityLease(socketPath: "/private/tmp/wendy/identity.sock")
        #expect(lease.socketPath == "/private/tmp/wendy/identity.sock")

        #expect(throws: NativeAppIdentityError.invalidSocketPath) {
            _ = try NativeAppIdentityLease(socketPath: "identity.sock")
        }
        #expect(throws: NativeAppIdentityError.invalidSocketPath) {
            _ = try NativeAppIdentityLease(socketPath: "/")
        }
        #expect(throws: NativeAppIdentityError.invalidSocketPath) {
            _ = try NativeAppIdentityLease(socketPath: "/private/tmp/wendy/../identity.sock")
        }
        #expect(throws: NativeAppIdentityError.invalidSocketPath) {
            _ = try NativeAppIdentityLease(socketPath: "/private/tmp/wendy/identity\n.sock")
        }
        #expect(throws: NativeAppIdentityError.invalidSocketPath) {
            _ = try NativeAppIdentityLease(socketPath: "/" + String(repeating: "a", count: 103))
        }
    }

    @Test
    func `unavailable identity removes inherited and requested socket paths`() {
        let key = NativeAppIdentityLease.environmentKey
        let unavailable = ContainerService.nativeAppEnvironment(
            appName: "sh.wendy.test",
            otelPort: 4317,
            source: [key: "/tmp/inherited.sock"],
            overrides: [key: "/tmp/requested.sock"]
        )
        #expect(unavailable[key] == nil)

        let bound = ContainerService.nativeAppEnvironment(
            appName: "sh.wendy.test",
            otelPort: 4317,
            source: [key: "/tmp/inherited.sock"],
            overrides: [key: "/tmp/requested.sock"],
            identitySocketPath: "/private/tmp/provider.sock"
        )
        #expect(bound[key] == "/private/tmp/provider.sock")
    }

    @Test
    func `native launch activates and revokes a process-bound identity lease`() async throws {
        let root = try makeIdentityTestDirectory()
        defer { try? FileManager.default.removeItem(at: root) }

        let appID = "sh.wendy.tests.IdentityLease"
        let appDirectory = root.appendingPathComponent(appID, isDirectory: true)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeIdentityProbe(to: appDirectory.appendingPathComponent("identity-probe.sh"))

        let expectedSocket = "/private/tmp/wendy-\(UUID().uuidString).sock"
        let provider = try RecordingNativeAppIdentityProvider(socketPath: expectedSocket)
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: root,
            nativeAppIdentityProvider: provider
        )

        try await registerIdentityTestApp(service: service, appID: appID)
        try await startIdentityTestApp(service: service, appID: appID)
        try await waitForIdentityTestFile(
            appDirectory.appendingPathComponent("identity-socket.txt")
        )

        let recordedSocket = try String(
            contentsOf: appDirectory.appendingPathComponent("identity-socket.txt"),
            encoding: .utf8
        )
        #expect(recordedSocket == expectedSocket)

        let runningEvents = await provider.events
        try await stopIdentityTestApp(service: service, appID: appID)
        let stoppedEvents = await provider.events

        #expect(runningEvents.count == 2)
        #expect(runningEvents[0] == .prepared(appID))
        guard case .activated(let leaseID, let pid) = runningEvents[1] else {
            Issue.record("expected an activated identity lease")
            return
        }
        #expect(pid > 0)
        #expect(stoppedEvents.last == .revoked(leaseID))
    }

    @Test
    func `identity activation failure stops the process and revokes the lease`() async throws {
        let root = try makeIdentityTestDirectory()
        defer { try? FileManager.default.removeItem(at: root) }

        let appID = "sh.wendy.tests.IdentityActivationFailure"
        let appDirectory = root.appendingPathComponent(appID, isDirectory: true)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeIdentityProbe(to: appDirectory.appendingPathComponent("identity-probe.sh"))

        let provider = try RecordingNativeAppIdentityProvider(
            socketPath: "/private/tmp/wendy-\(UUID().uuidString).sock",
            failActivation: true
        )
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: root,
            nativeAppIdentityProvider: provider
        )

        try await registerIdentityTestApp(service: service, appID: appID)
        await #expect(throws: RPCError.self) {
            try await startIdentityTestApp(service: service, appID: appID)
        }

        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
        let events = await provider.events
        guard case .activated(let leaseID, _) = events.dropFirst().first else {
            Issue.record("expected identity activation to be attempted")
            return
        }
        #expect(events.last == .revoked(leaseID))
    }
}

private actor RecordingNativeAppIdentityProvider: NativeAppIdentityProviding {
    enum Event: Equatable, Sendable {
        case prepared(String)
        case activated(UUID, Int32)
        case revoked(UUID)
    }

    private(set) var events: [Event] = []
    private let failActivation: Bool
    private let lease: NativeAppIdentityLease

    init(socketPath: String, failActivation: Bool = false) throws {
        self.lease = try NativeAppIdentityLease(socketPath: socketPath)
        self.failActivation = failActivation
    }

    func prepareIdentity(forAppID appID: String) async throws -> NativeAppIdentityLease? {
        self.events.append(.prepared(appID))
        return self.lease
    }

    func activateIdentity(_ lease: NativeAppIdentityLease, forProcessID pid: Int32) async throws {
        self.events.append(.activated(lease.id, pid))
        if self.failActivation {
            throw IdentityProviderTestError.activationFailed
        }
    }

    func revokeIdentity(_ lease: NativeAppIdentityLease) async {
        self.events.append(.revoked(lease.id))
    }
}

private enum IdentityProviderTestError: Error {
    case activationFailed
}

private func registerIdentityTestApp(service: ContainerService, appID: String) async throws {
    var request = Wendy_Agent_Services_V1_CreateContainerRequest()
    request.appName = appID
    request.cmd = "identity-probe.sh"
    _ = try await service.createContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: identityTestServerContext(method: "CreateContainer")
    )
}

private func startIdentityTestApp(service: ContainerService, appID: String) async throws {
    var request = Wendy_Agent_Services_V1_StartContainerRequest()
    request.appName = appID
    _ = try await service.startContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: identityTestServerContext(method: "StartContainer")
    )
}

private func stopIdentityTestApp(service: ContainerService, appID: String) async throws {
    var request = Wendy_Agent_Services_V1_StopContainerRequest()
    request.appName = appID
    _ = try await service.stopContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: identityTestServerContext(method: "StopContainer")
    )
}

private func identityTestServerContext(method: String) -> ServerContext {
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

private func makeIdentityTestDirectory() throws -> URL {
    let url = FileManager.default.temporaryDirectory.appendingPathComponent(
        "wendy-identity-test-\(UUID().uuidString)",
        isDirectory: true
    )
    try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    return url
}

private func writeIdentityProbe(to url: URL) throws {
    try
        "#!/bin/sh\nprintf '%s' \"$WENDY_APP_IDENTITY_SOCKET\" > identity-socket.txt\ntrap 'exit 0' TERM\nwhile true; do sleep 1; done\n"
        .write(to: url, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func waitForIdentityTestFile(_ url: URL) async throws {
    let clock = ContinuousClock()
    let deadline = clock.now + .seconds(5)
    while clock.now < deadline {
        if FileManager.default.fileExists(atPath: url.path) { return }
        try await Task.sleep(for: .milliseconds(20))
    }
    throw IdentityProviderTestError.activationFailed
}
