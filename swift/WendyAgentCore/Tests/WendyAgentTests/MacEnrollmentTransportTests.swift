import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

/// Actual CLI/process/RPC/ACME/persistence/mTLS path. The CA/Cloud peer is local,
/// and the Agent uses software keys and private state, never the user Keychain.
@Suite("Mac enrollment production transport")
struct MacEnrollmentTransportTests {
    @Test(
        .enabled(if: ProcessInfo.processInfo.environment["WENDY_ENROLLMENT_CLI"] != nil),
        .timeLimit(.minutes(2))
    )
    func enrollAndRestartWithUnavailableServiceControls() async throws {
        let cli = try #require(ProcessInfo.processInfo.environment["WENDY_ENROLLMENT_CLI"])
        let helper = try #require(ProcessInfo.processInfo.environment["WENDY_ENROLLMENT_PEER"])
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "wendy-mac-enrollment-\(UUID())"
        )
        try FileManager.default.createDirectory(
            at: root,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        var removable = true
        defer {
            if removable {
                try? FileManager.default.removeItem(at: root)
            } else {
                print("MAC_ENROLLMENT_CLEANUP_INCOMPLETE retained \(root.path)")
            }
        }
        let peerRoot = root.appendingPathComponent("peer")
        let environment = [
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "HOME": root.path,
            "WENDY_CONFIG_DIR": peerRoot.appendingPathComponent("cli").path,
            "WENDY_SECRET_STORE": "file", "WENDY_TLS_SESSION_STORE": "off", "WENDY_TLS_DEBUG": "1",
            "WENDY_CLOUD_INSECURE": "true",
            "CI": "1", "WENDY_ANALYTICS": "false", "WENDY_NO_UPDATE_CHECK": "1", "TERM": "dumb",
        ]
        let peerLog = root.appendingPathComponent("peer.log")
        let peer = try enrollmentProcess(helper, [peerRoot.path], environment, peerLog)
        var agents: [WendyAgent] = []
        do {
            let readyFile = peerRoot.appendingPathComponent("ready.json")
            try await enrollmentWait("local protocol peer", seconds: 10) {
                FileManager.default.fileExists(atPath: readyFile.path)
            }
            let ready = try JSONDecoder().decode(
                EnrollmentPeer.self,
                from: Data(contentsOf: readyFile)
            )
            func enroll(_ address: String, _ label: String) async throws -> (Int32, String) {
                let log = root.appendingPathComponent(label + ".log")
                let process = try enrollmentProcess(
                    cli,
                    [
                        "--device", address, "--json", "device", "enroll", "--name",
                        "mac-enrollment-fixture", "--acme-directory-url", ready.directory,
                    ],
                    environment,
                    log
                )
                do {
                    try await enrollmentWait("CLI \(label)", seconds: 35) { !process.isRunning }
                    process.waitUntilExit()
                } catch {
                    if !(await enrollmentStop(process)) { removable = false }
                    throw error
                }
                let output = try String(contentsOf: log, encoding: .utf8)
                print("MAC_ENROLLMENT_CLI_\(label) status=\(process.terminationStatus) \(output)")
                return (process.terminationStatus, output)
            }
            func counters(_ expected: Int) async throws -> EnrollmentCounters {
                var request = URLRequest(url: try #require(URL(string: ready.status)))
                request.timeoutInterval = 3
                let (bytes, response) = try await URLSession.shared.data(for: request)
                try #require((response as? HTTPURLResponse)?.statusCode == 200)
                let value = try JSONDecoder().decode(EnrollmentCounters.self, from: bytes)
                try #require(
                    value.enrolled == expected && value.accounts == expected
                        && value.orders == expected && value.finalizes == expected
                )
                try #require(value.errors?.isEmpty != false)
                return value
            }
            let old = try await enroll(ready.legacy, "legacy")
            try #require(
                old.0 != 0 && old.1.contains("does not support"),
                "legacy service did not refuse before EAB"
            )
            _ = try await counters(0)
            print("MAC_ENROLLMENT_LEGACY_REFUSED before Cloud mint")

            func agent(_ directory: URL, unavailable: Bool = false) -> WendyAgent {
                WendyAgent(
                    configuration: .init(port: 18820, otelPort: 0),
                    probesLinuxContainerRuntime: false,
                    reportedVersion: { "mac-enrollment-fixture" },
                    stateDirectory: directory,
                    makeVideoService: {
                        VideoService(camera: EnrollmentCamera(), controls: EnrollmentControls())
                    },
                    makeProvisioningService: {
                        ProvisioningService(
                            configPath: $0,
                            isSecureEnclaveAvailable: { false },
                            acmeEnrollmentUnavailableReason: {
                                unavailable
                                    ? "fixture unavailable enrollment capability"
                                    : ProvisioningService.hostACMEEnrollmentUnavailableReason()
                            }
                        )
                    }
                )
            }
            let unavailable = agent(root.appendingPathComponent("unavailable"), unavailable: true)
            agents.append(unavailable)
            try await unavailable.start()
            let refused = try await enroll(
                "127.0.0.1:\(try #require(await unavailable.listeningPort))",
                "unavailable"
            )
            try #require(
                refused.0 != 0 && refused.1.contains("fixture unavailable enrollment capability")
            )
            _ = try await counters(0)
            await unavailable.stop()
            print("MAC_ENROLLMENT_UNAVAILABLE_REFUSED before Cloud mint")

            let state = root.appendingPathComponent("agent")
            let first = agent(state)
            agents.append(first)
            try await first.start()
            let plaintextPort = try #require(await first.listeningPort)
            // The real CLI reconnects to setup-port+1. Refuse an occupied pair,
            // rather than silently verifying some other agent's listener.
            try #require(plaintextPort == 18820, "fixture setup port occupied")
            let result = try await enroll("127.0.0.1:\(plaintextPort)", "normal")
            try #require(result.0 == 0, "actual CLI enrollment failed: \(result.1)")
            let issued = try await counters(1)
            let saved = try #require(ProvisioningStore(configPath: state).load())
            let principal =
                "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/\(issued.device)"
            try #require(
                saved.enrolled && saved.principalURI == principal && saved.cloudHost == ready.cloud
            )
            guard case .softwarePEM(let key) = saved.keyBacking else {
                throw RPCError(code: .internalError, message: "fixture touched Secure Enclave")
            }
            try #require(!key.isEmpty && !saved.certPEM.isEmpty)
            let before = try Data(contentsOf: state.appendingPathComponent("provisioning.json"))
            let binding = try String(
                contentsOf: peerRoot.appendingPathComponent("cli/config.json"),
                encoding: .utf8
            )
            try #require(binding.contains(principal), "CLI did not save verified device binding")
            try #require(await first.listeningPort == plaintextPort + 1)
            await first.stop()
            try #require(await first.listeningPort == nil)
            try #require(await first.status == .stopped)

            let restarted = agent(state)
            agents.append(restarted)
            try await restarted.start()
            let securePort = try #require(await restarted.listeningPort)
            try #require(securePort == plaintextPort + 1)
            var options = CallOptions.defaults
            options.timeout = .seconds(3)
            let transport = try HTTP2ClientTransport.Posix(
                target: .ipv4(address: "127.0.0.1", port: securePort),
                transportSecurity: .mTLS(
                    certificateChain: [.bytes(Array(ready.operator.utf8), format: .pem)],
                    privateKey: .bytes(Array(ready.key.utf8), format: .pem)
                ) {
                    $0.trustRoots = .certificates([.bytes(Array(ready.ca.utf8), format: .pem)])
                    $0.serverCertificateVerification = .noHostnameVerification
                }
            )
            try await withGRPCClient(transport: transport) { client in
                let status = try await Wendy_Agent_Services_V2_WendyProvisioningService.Client(
                    wrapping: client
                ).isProvisioned(.init(), options: options)
                try #require(
                    status.provisioned.principalUri == principal
                        && status.provisioned.assetUuid == issued.device
                )
            }
            let rejected = try HTTP2ClientTransport.Posix(
                target: .ipv4(address: "127.0.0.1", port: securePort),
                transportSecurity: .tls {
                    $0.trustRoots = .certificates([.bytes(Array(ready.ca.utf8), format: .pem)])
                    $0.serverCertificateVerification = .noHostnameVerification
                }
            )
            try await withGRPCClient(transport: rejected) { client in
                do {
                    _ = try await Wendy_Agent_Services_V2_WendyProvisioningService.Client(
                        wrapping: client
                    ).isProvisioned(.init(), options: options)
                    Issue.record("restarted enrolled agent admitted an unauthenticated client")
                } catch let error as RPCError { #expect(error.code == .unavailable) }
            }
            try #require(
                try Data(contentsOf: state.appendingPathComponent("provisioning.json")) == before
            )
            _ = try await counters(1)
            for agent in agents { await agent.stop() }
            removable = await enrollmentStop(peer)
            try #require(removable && peer.terminationStatus == 0)
            print(
                "MAC_ENROLLMENT_VERIFIED actual CLI, ACME EAB/CSR, persisted UUID, mTLS restart and no-client-certificate refusal"
            )
        } catch {
            for agent in agents { await agent.stop() }
            if !(await enrollmentStop(peer)) { removable = false }
            if let log = try? String(contentsOf: peerLog, encoding: .utf8) {
                print("MAC_ENROLLMENT_PEER_LOG \(log)")
            }
            throw error
        }
    }
}

private struct EnrollmentPeer: Decodable {
    let directory, status, cloud, legacy, ca, `operator`, key: String
}
private struct EnrollmentCounters: Decodable {
    let enrolled, accounts, orders, finalizes: Int
    let device: String
    let errors: [String]?
}
private func enrollmentProcess(
    _ executable: String,
    _ arguments: [String],
    _ environment: [String: String],
    _ log: URL
) throws -> Process {
    FileManager.default.createFile(atPath: log.path, contents: nil)
    let file = try FileHandle(forWritingTo: log)
    defer { try? file.close() }
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = arguments
    process.environment = environment
    process.standardInput = FileHandle.nullDevice
    process.standardOutput = file
    process.standardError = file
    try process.run()
    return process
}
private func enrollmentWait(
    _ reason: String,
    seconds: Int,
    _ predicate: () throws -> Bool
) async throws {
    let deadline = ContinuousClock.now + .seconds(seconds)
    while !(try predicate()) && ContinuousClock.now < deadline {
        try await Task.sleep(for: .milliseconds(40))
    }
    try #require(try predicate(), "enrollment boundary timed out: \(reason)")
}
private func enrollmentStop(_ process: Process) async -> Bool {
    if process.isRunning { process.terminate() }
    let deadline = ContinuousClock.now + .seconds(5)
    while process.isRunning && ContinuousClock.now < deadline {
        try? await Task.sleep(for: .milliseconds(40))
    }
    guard !process.isRunning else { return false }
    process.waitUntilExit()
    return true
}
private struct EnrollmentCamera: CameraManaging {
    func devices() async -> [CameraDeviceInfo] { [] }
    func frames(for device: CameraDeviceInfo) -> AsyncThrowingStream<CameraFrame, any Error> {
        AsyncThrowingStream { $0.finish() }
    }
}
private struct EnrollmentControls: CameraControlManaging {
    func list(uniqueID: String) async -> [UVCCameraControl] { [] }
    func set(
        uniqueID: String,
        values: [(String, Int32)],
        persist: Bool
    ) async -> [CameraControlOutcome] { [] }
    func reset(uniqueID: String, names: [String]) async -> [CameraControlOutcome] { [] }
    func restore(uniqueID: String) async {}
}
