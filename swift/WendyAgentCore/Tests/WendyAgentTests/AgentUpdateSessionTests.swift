import CryptoKit
import Foundation
import GRPCCore
import Logging
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("AgentUpdateSession")
struct AgentUpdateSessionTests {

    // MARK: - Happy path

    @Test("commits the update, schedules the relaunch before acking, and cleans up staging")
    func happyPath() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        try await runSession(
            [
                chunkRequest(payload.prefix(6)),
                chunkRequest(payload.dropFirst(6)),
                updateRequest(sha256: payloadSHA256),
            ],
            deps: makeDependencies(harness),
            log: harness.log
        )

        // The relaunch must be scheduled before the ack: a client that hangs
        // up must not leave the agent installed-but-never-restarted.
        #expect(harness.log.events() == ["extract", "replace", "relaunch", "terminate", "ack"])
        let responses = harness.log.responses()
        #expect(responses.count == 1)
        #expect(
            responses.first?.responseType
                == .updated(Wendy_Agent_Services_V1_UpdateAgentResponse.Updated())
        )
        // Chunks were streamed to disk verbatim.
        #expect(harness.log.payloadSeen() == payload)
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test("the default signature checker accepts an unsigned payload (verification disabled)")
    func defaultSignatureCheckerPasses() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness, signature: DefaultUpdateSignatureChecker()),
            log: harness.log
        )

        #expect(harness.log.events().contains("replace"))
        #expect(harness.log.responses().count == 1)
    }

    @Test("a failed ack still commits: the session returns and the relaunch is scheduled")
    func ackFailureStillCommits() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness),
            log: harness.log,
            ackThrows: true
        )

        #expect(harness.log.events() == ["extract", "replace", "relaunch", "terminate", "ack"])
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test("the commit hook fires before the relaunch and termination are scheduled")
    func commitHookRunsBeforeTermination() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        var deps = makeDependencies(harness)
        let log = harness.log
        deps.onCommitted = { log.record("commit") }

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: deps,
            log: harness.log
        )

        // The lock must be pinned as committed before anything can tear the
        // process down, so a stale-lock steal can never race the shutdown.
        let expected = ["extract", "replace", "commit", "relaunch", "terminate", "ack"]
        #expect(harness.log.events() == expected)
    }

    @Test("the commit hook does not fire when the update fails")
    func commitHookSkippedOnFailure() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        var deps = makeDependencies(harness)
        let log = harness.log
        deps.onCommitted = { log.record("commit") }

        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: String(repeating: "a", count: 64))],
            deps: deps,
            log: harness.log
        )

        #expect(error?.code == .dataLoss)
        #expect(!harness.log.events().contains("commit"))
    }

    @Test("the payload is extracted into a subdirectory, never over the payload itself")
    func extractionUsesDedicatedSubdirectory() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness),
            log: harness.log
        )

        let extract = try #require(harness.log.extractSeen())
        #expect(extract.into.lastPathComponent == "extract")
        #expect(extract.zip.lastPathComponent == "payload.zip")
        // Both live directly under the session directory, so an archive entry
        // named `payload.zip` cannot clobber the source mid-extract.
        #expect(
            extract.zip.deletingLastPathComponent().standardizedFileURL
                == extract.into.deletingLastPathComponent().standardizedFileURL
        )
        let extractRoot = extract.into.standardizedFileURL.path
        #expect(!extract.zip.standardizedFileURL.path.hasPrefix(extractRoot))
    }

    @Test("a relaunch that cannot be scheduled still terminates and acks")
    func relaunchFailureStillCommits() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness, relaunchThrows: true),
            log: harness.log
        )

        #expect(harness.log.events() == ["extract", "replace", "relaunch", "terminate", "ack"])
    }

    // MARK: - Digest validation

    @Test("a SHA256 mismatch fails with dataLoss and never touches the installed bundle")
    func shaMismatch() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: String(repeating: "a", count: 64))],
            deps: makeDependencies(harness),
            log: harness.log
        )

        #expect(error?.code == .dataLoss)
        #expect(error?.message.contains("SHA256 mismatch") == true)
        #expect(error?.message.contains(payloadSHA256) == true)
        #expect(harness.log.events().isEmpty)
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test(
        "a malformed SHA256 fails with invalidArgument",
        arguments: [
            "",
            String(repeating: "a", count: 63),
            String(repeating: "a", count: 65),
            String(repeating: "z", count: 64),
            "not-a-digest",
        ]
    )
    func malformedSHA(_ sha: String) async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: sha)],
            deps: makeDependencies(harness),
            log: harness.log
        )

        #expect(error?.code == .invalidArgument)
        #expect(error?.message == "update control command is missing a valid SHA256")
        #expect(harness.log.events().isEmpty)
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test("an uppercase SHA256 is accepted")
    func uppercaseSHAAccepted() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256.uppercased())],
            deps: makeDependencies(harness),
            log: harness.log
        )

        #expect(harness.log.responses().count == 1)
    }

    // MARK: - Stream framing

    @Test("a payload larger than the cap fails with resourceExhausted")
    func oversizePayload() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let error = await runSessionExpectingFailure(
            [
                chunkRequest(Data(repeating: 0x41, count: 3)),
                chunkRequest(Data(repeating: 0x41, count: 3)),
                updateRequest(sha256: payloadSHA256),
            ],
            deps: makeDependencies(harness, maxPayloadBytes: 4),
            log: harness.log
        )

        #expect(error?.code == .resourceExhausted)
        #expect(error?.message.contains("exceeds maximum agent bundle size") == true)
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test("a stream that ends without a control command fails with invalidArgument")
    func missingControlCommand() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let error = await runSessionExpectingFailure(
            [chunkRequest(payload)],
            deps: makeDependencies(harness),
            log: harness.log
        )

        #expect(error?.code == .invalidArgument)
        #expect(error?.message == "update stream ended without update control command")
        #expect(harness.log.events().isEmpty)
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test("an empty control command fails with invalidArgument")
    func emptyControlCommand() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        var request = Wendy_Agent_Services_V1_UpdateAgentRequest()
        request.requestType = .control(Wendy_Agent_Services_V1_UpdateAgentRequest.ControlCommand())

        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), request],
            deps: makeDependencies(harness),
            log: harness.log
        )

        #expect(error?.code == .invalidArgument)
        #expect(harness.stagingEntries().isEmpty)
    }

    // MARK: - Installer error mapping

    @Test("extractBundle errors map to RPC codes")
    func extractErrorMapping() async throws {
        let cases: [(AgentBundleInstallerError, RPCError.Code, String)] = [
            (.notAnAppArchive("ditto failed"), .invalidArgument, "not a valid app archive"),
            (
                .invalidBundle("missing WLWendyAgentVersion"), .invalidArgument,
                "WLWendyAgentVersion"
            ),
        ]

        for (installerError, expectedCode, expectedFragment) in cases {
            let harness = try Harness()
            defer { harness.cleanup() }

            let installer = StubInstaller(
                log: harness.log,
                stagedApp: harness.stagedApp,
                extractError: installerError
            )
            let error = await runSessionExpectingFailure(
                [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
                deps: makeDependencies(harness, installer: installer),
                log: harness.log
            )

            #expect(error?.code == expectedCode)
            #expect(error?.message.contains(expectedFragment) == true)
            #expect(harness.log.events() == ["extract"])
            #expect(harness.stagingEntries().isEmpty)
        }
    }

    @Test("replaceBundle errors map to RPC codes and pass their detail through")
    func replaceErrorMapping() async throws {
        let cases: [(AgentBundleInstallerError, RPCError.Code, String)] = [
            (.parentNotWritable("directory is not writable"), .permissionDenied, "not writable"),
            (
                .swapFailed("failed to install new bundle at /x: boom; previous version restored"),
                .internalError, "previous version restored"
            ),
        ]

        for (installerError, expectedCode, expectedFragment) in cases {
            let harness = try Harness()
            defer { harness.cleanup() }

            let installer = StubInstaller(
                log: harness.log,
                stagedApp: harness.stagedApp,
                replaceError: installerError
            )
            let error = await runSessionExpectingFailure(
                [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
                deps: makeDependencies(harness, installer: installer),
                log: harness.log
            )

            #expect(error?.code == expectedCode)
            #expect(error?.message.contains(expectedFragment) == true)
            #expect(harness.log.events() == ["extract", "replace"])
            #expect(harness.stagingEntries().isEmpty)
        }
    }

    @Test("a non-installer failure inside the installer surfaces as internalError")
    func unexpectedInstallerFailure() async throws {
        for stage in [StubInstaller.Stage.extract, .replace] {
            let harness = try Harness()
            defer { harness.cleanup() }

            let installer = StubInstaller(
                log: harness.log,
                stagedApp: harness.stagedApp,
                unexpectedFailure: stage
            )
            let error = await runSessionExpectingFailure(
                [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
                deps: makeDependencies(harness, installer: installer),
                log: harness.log
            )

            #expect(error?.code == .internalError)
            #expect(harness.stagingEntries().isEmpty)
        }
    }

    // MARK: - Code signature gating

    @Test("an incoming bundle with an invalid signature fails with failedPrecondition")
    func incomingInvalidSignature() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let codesign = StubCodesignVerifier(
            results: [
                harness.stagedApp.path: .failure(.invalidSignature("code object is not signed"))
            ]
        )
        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness, codesign: codesign),
            log: harness.log
        )

        #expect(error?.code == .failedPrecondition)
        #expect(
            error?.message.contains("incoming agent bundle failed code signature verification")
                == true
        )
        #expect(error?.message.contains("code object is not signed") == true)
        #expect(harness.log.events() == ["extract"])
    }

    @Test("an incoming bundle that cannot be inspected fails with internalError")
    func incomingInspectionFailure() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let codesign = StubCodesignVerifier(
            results: [harness.stagedApp.path: .failure(.inspectionFailed("SecStaticCode failed"))]
        )
        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness, codesign: codesign),
            log: harness.log
        )

        #expect(error?.code == .internalError)
        #expect(error?.message.contains("could not inspect the incoming agent bundle") == true)
    }

    @Test("a running bundle that fails verification is an internalError, not a client error")
    func runningBundleInspectionFailure() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        // `.invalidSignature` on the RUNNING bundle must not masquerade as the
        // client having sent an unsigned payload.
        let codesign = StubCodesignVerifier(
            results: [harness.currentBundle.path: .failure(.invalidSignature("running is ad-hoc"))]
        )
        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness, codesign: codesign),
            log: harness.log
        )

        #expect(error?.code == .internalError)
        #expect(error?.message.contains("could not inspect the running agent bundle") == true)
        #expect(error?.message.contains("running is ad-hoc") == true)
        #expect(harness.log.events() == ["extract"])
        #expect(harness.stagingEntries().isEmpty)
    }

    @Test("a code signature policy rejection propagates its RPCError")
    func policyRejectionPropagates() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let codesign = StubCodesignVerifier(
            results: [
                harness.stagedApp.path: .success(
                    CodesignInfo(
                        teamID: "TEAM2",
                        isDeveloperID: true,
                        bundleIdentifier: "sh.wendy.someone-else"
                    )
                ),
                harness.currentBundle.path: .success(
                    CodesignInfo(
                        teamID: "TEAM1",
                        isDeveloperID: true,
                        bundleIdentifier: "sh.wendy.agent"
                    )
                ),
            ]
        )
        let error = await runSessionExpectingFailure(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness, codesign: codesign),
            log: harness.log
        )

        #expect(error?.code == .failedPrecondition)
        #expect(error?.message.contains("does not match") == true)
        #expect(harness.log.events() == ["extract"])
        #expect(harness.stagingEntries().isEmpty)
    }

    // MARK: - Signature seam

    @Test("signature verification failures map to RPC codes")
    func signatureErrorMapping() async throws {
        let cases: [(any Error, RPCError.Code, String)] = [
            (
                SignatureVerifierError.unsigned, .failedPrecondition,
                "agent update is unsigned; refusing install"
            ),
            (
                SignatureVerifierError.badSignature, .dataLoss,
                "agent update signature verification failed; refusing install"
            ),
            (
                StubError.signatureBroken, .internalError,
                "agent update signature verification error"
            ),
        ]

        for (thrownError, expectedCode, expectedFragment) in cases {
            let harness = try Harness()
            defer { harness.cleanup() }

            let checker = StubSignatureChecker { _, _ in throw thrownError }
            let error = await runSessionExpectingFailure(
                [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
                deps: makeDependencies(harness, signature: checker),
                log: harness.log
            )

            #expect(error?.code == expectedCode)
            #expect(error?.message.contains(expectedFragment) == true)
            #expect(harness.log.events().isEmpty)
            #expect(harness.stagingEntries().isEmpty)
        }
    }

    @Test("the digest and signature reach the verifier")
    func signatureInputs() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let expectedDigest = Data(SHA256.hash(data: payload))
        let signature = Data([0x01, 0x02, 0x03])
        let checker = StubSignatureChecker { digest, receivedSignature in
            #expect(digest == expectedDigest)
            #expect(receivedSignature == signature)
        }

        try await runSession(
            [
                chunkRequest(payload),
                updateRequest(sha256: payloadSHA256, signature: signature),
            ],
            deps: makeDependencies(harness, signature: checker),
            log: harness.log
        )
    }

    // MARK: - Staging hygiene

    @Test("stale staging directories from earlier attempts are wiped")
    func staleSessionsWiped() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let stale = harness.stagingRoot.appendingPathComponent("stale-session")
        try FileManager.default.createDirectory(at: stale, withIntermediateDirectories: true)
        try Data("leftover".utf8).write(to: stale.appendingPathComponent("payload.zip"))

        try await runSession(
            [chunkRequest(payload), updateRequest(sha256: payloadSHA256)],
            deps: makeDependencies(harness),
            log: harness.log
        )

        #expect(harness.stagingEntries().isEmpty)
    }
}

// MARK: - Lock behavior

// Lock semantics in isolation live in `AgentUpdateLockTests`
// (AgentUpdatePlatformTests.swift); these cover how the handler uses it.
@Suite("AgentService.updateAgent locking")
struct AgentServiceUpdateLockingTests {
    @Test("a second concurrent updateAgent call is rejected with failedPrecondition")
    func concurrentUpdatesRejected() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let lock = AgentUpdateLock()
        let service = AgentService(
            updateLock: lock,
            updateDependencies: makeDependencies(harness)
        )

        // The first call acquires the lock and returns its (not yet driven)
        // streaming response, so the update is still in flight.
        _ = try await service.updateAgent(
            request: makeStreamingRequest([chunkRequest(payload)]),
            context: makeUpdateContext()
        )

        do {
            _ = try await service.updateAgent(
                request: makeStreamingRequest([chunkRequest(payload)]),
                context: makeUpdateContext()
            )
            Issue.record("Expected the second updateAgent call to be rejected")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            #expect(error.message == "an update is already in progress")
        }
    }

    @Test("a failed update releases the lock so the client can retry")
    func failedUpdateReleasesLock() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let lock = AgentUpdateLock()
        let service = AgentService(
            updateLock: lock,
            updateDependencies: makeDependencies(harness)
        )

        // No control command: the session fails.
        let response = try await service.updateAgent(
            request: makeStreamingRequest([chunkRequest(payload)]),
            context: makeUpdateContext()
        )
        let writer = CollectingWriter<Wendy_Agent_Services_V1_UpdateAgentResponse>()
        await #expect(throws: RPCError.self) {
            _ = try await response.accepted.get().producer(RPCWriter(wrapping: writer))
        }

        #expect(await lock.tryAcquire())
    }

    @Test("a committed update keeps the lock held so retries see an update in progress")
    func committedUpdateKeepsLock() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let lock = AgentUpdateLock()
        let service = AgentService(
            updateLock: lock,
            updateDependencies: makeDependencies(harness)
        )

        let response = try await service.updateAgent(
            request: makeStreamingRequest([
                chunkRequest(payload),
                updateRequest(sha256: payloadSHA256),
            ]),
            context: makeUpdateContext()
        )
        let writer = CollectingWriter<Wendy_Agent_Services_V1_UpdateAgentResponse>()
        _ = try await response.accepted.get().producer(RPCWriter(wrapping: writer))

        #expect(writer.snapshot().count == 1)
        #expect(harness.log.events().contains("relaunch"))
        #expect(await lock.tryAcquire() == false)
    }

    @Test("a lock leaked before the response producer runs is stolen once it goes stale")
    func leakedLockIsStolenWhenStale() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let clock = AgentUpdateTestClock()
        let lock = AgentUpdateLock(now: clock.now)
        let service = AgentService(
            updateLock: lock,
            updateDependencies: makeDependencies(harness)
        )

        // grpc-swift writes the response's initial metadata *before* running
        // the producer; if the client aborts in that window the producer — and
        // the `release` in its catch — never runs. Dropping the response here
        // models exactly that leak.
        _ = try await service.updateAgent(
            request: makeStreamingRequest([chunkRequest(payload)]),
            context: makeUpdateContext()
        )
        #expect(await lock.tryAcquire() == false)

        clock.advance(by: AgentUpdateLock.defaultStaleThreshold)
        // Without the steal, every later update would fail with
        // failedPrecondition until someone restarted the app by hand.
        #expect(await lock.tryAcquire() == true)
    }

    @Test("a committed update pins the lock so it is never stolen")
    func committedUpdateIsNeverStolen() async throws {
        let harness = try Harness()
        defer { harness.cleanup() }

        let clock = AgentUpdateTestClock()
        let lock = AgentUpdateLock(now: clock.now)
        let service = AgentService(
            updateLock: lock,
            updateDependencies: makeDependencies(harness)
        )

        let response = try await service.updateAgent(
            request: makeStreamingRequest([
                chunkRequest(payload),
                updateRequest(sha256: payloadSHA256),
            ]),
            context: makeUpdateContext()
        )
        let writer = CollectingWriter<Wendy_Agent_Services_V1_UpdateAgentResponse>()
        _ = try await response.accepted.get().producer(RPCWriter(wrapping: writer))

        clock.advance(by: .seconds(365 * 24 * 60 * 60))
        // The handler wires the session's commit hook to markCommitted(), so
        // the shutdown window can never be raced by a second install.
        #expect(await lock.tryAcquire() == false)
    }
}

// MARK: - Fixtures

private let payload = Data("fake-agent-bundle-zip".utf8)
private let payloadSHA256 = Data(SHA256.hash(data: payload)).map { String(format: "%02x", $0) }
    .joined()

private enum StubError: Error {
    case ackFailed
    case relaunchFailed
    case signatureBroken
    case installerBroken
}

/// Ordered record of the side effects a session performs, plus the responses
/// it writes. A lock-guarded class (not an actor) because
/// `AgentRelaunchScheduling` is synchronous.
private final class SessionLog: @unchecked Sendable {
    private let queue = DispatchQueue(label: "wendy.tests.agent-update-log")
    private var recordedEvents: [String] = []
    private var recordedResponses: [Wendy_Agent_Services_V1_UpdateAgentResponse] = []
    private var recordedPayload: Data?
    private var recordedExtract: (zip: URL, into: URL)?

    func record(_ event: String) {
        self.queue.sync { self.recordedEvents.append(event) }
    }
    func recordExtract(zip: URL, into directory: URL) {
        self.queue.sync { self.recordedExtract = (zip: zip, into: directory) }
    }
    func extractSeen() -> (zip: URL, into: URL)? { self.queue.sync { self.recordedExtract } }
    func append(_ response: Wendy_Agent_Services_V1_UpdateAgentResponse) {
        self.queue.sync { self.recordedResponses.append(response) }
    }
    func recordPayload(_ data: Data?) {
        self.queue.sync { self.recordedPayload = data }
    }
    func events() -> [String] { self.queue.sync { self.recordedEvents } }
    func responses() -> [Wendy_Agent_Services_V1_UpdateAgentResponse] {
        self.queue.sync { self.recordedResponses }
    }
    func payloadSeen() -> Data? { self.queue.sync { self.recordedPayload } }
}

private struct Harness {
    let root: URL
    let stagingRoot: URL
    let currentBundle: URL
    let stagedApp: URL
    let log = SessionLog()

    init() throws {
        self.root = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-agent-update-test-\(UUID().uuidString)")
        self.stagingRoot = self.root.appendingPathComponent("staging")
        self.currentBundle = self.root.appendingPathComponent("Applications/WendyAgentMac.app")
        self.stagedApp = self.root.appendingPathComponent("staged/WendyAgentMac.app")

        try FileManager.default.createDirectory(
            at: self.stagingRoot,
            withIntermediateDirectories: true
        )
        try FileManager.default.createDirectory(
            at: self.stagedApp,
            withIntermediateDirectories: true
        )
        try FileManager.default.createDirectory(
            at: self.currentBundle.appendingPathComponent("Contents"),
            withIntermediateDirectories: true
        )
        let plist: [String: Any] = [
            "CFBundleIdentifier": "sh.wendy.agent",
            "WLWendyAgentVersion": "1.2.3",
        ]
        let data = try PropertyListSerialization.data(
            fromPropertyList: plist,
            format: .xml,
            options: 0
        )
        try data.write(to: self.currentBundle.appendingPathComponent("Contents/Info.plist"))
    }

    func cleanup() {
        try? FileManager.default.removeItem(at: self.root)
    }

    func stagingEntries() -> [URL] {
        (try? FileManager.default.contentsOfDirectory(
            at: self.stagingRoot,
            includingPropertiesForKeys: nil
        )) ?? []
    }
}

private struct StubSignatureChecker: UpdateSignatureChecking {
    let onVerify: @Sendable (Data, Data) throws -> Void

    init(_ onVerify: @escaping @Sendable (Data, Data) throws -> Void) {
        self.onVerify = onVerify
    }

    func verify(digest: Data, signature: Data) throws {
        try self.onVerify(digest, signature)
    }
}

private struct StubCodesignVerifier: CodesignVerifying {
    /// Keyed by bundle path; anything not listed gets `fallback`.
    var results: [String: Result<CodesignInfo, CodesignVerificationError>] = [:]
    var fallback: Result<CodesignInfo, CodesignVerificationError> = .success(
        CodesignInfo(teamID: "TEAM1", isDeveloperID: true, bundleIdentifier: "sh.wendy.agent")
    )

    func inspect(bundleAt url: URL) async throws -> CodesignInfo {
        try (self.results[url.path] ?? self.fallback).get()
    }
}

private struct StubInstaller: AgentBundleInstalling {
    /// Which call throws a non-`AgentBundleInstallerError` error.
    enum Stage: Sendable {
        case extract
        case replace
    }

    let log: SessionLog
    let stagedApp: URL
    var extractError: AgentBundleInstallerError?
    var replaceError: AgentBundleInstallerError?
    var unexpectedFailure: Stage?

    func extractBundle(zipAt zip: URL, into stagingDir: URL) async throws -> URL {
        self.log.recordPayload(FileManager.default.contents(atPath: zip.path))
        self.log.recordExtract(zip: zip, into: stagingDir)
        self.log.record("extract")
        if let extractError = self.extractError { throw extractError }
        if self.unexpectedFailure == .extract { throw StubError.installerBroken }
        return self.stagedApp
    }

    func replaceBundle(at destination: URL, with staged: URL) async throws {
        self.log.record("replace")
        if let replaceError = self.replaceError { throw replaceError }
        if self.unexpectedFailure == .replace { throw StubError.installerBroken }
    }
}

private struct SpyRelauncher: AgentRelaunchScheduling {
    let log: SessionLog
    var throwOnRelaunch = false

    func scheduleRelaunch(of bundleURL: URL) throws {
        self.log.record("relaunch")
        if self.throwOnRelaunch { throw StubError.relaunchFailed }
    }

    func scheduleTermination() {
        self.log.record("terminate")
    }
}

private final class CollectingWriter<Element: Sendable>: RPCWriterProtocol, @unchecked Sendable {
    private let queue = DispatchQueue(label: "wendy.tests.agent-update-writer")
    private var elements: [Element] = []

    func write(_ element: Element) async throws {
        self.queue.sync { self.elements.append(element) }
    }
    func write(contentsOf elements: some Sequence<Element>) async throws {
        self.queue.sync { self.elements.append(contentsOf: elements) }
    }
    func snapshot() -> [Element] {
        self.queue.sync { self.elements }
    }
}

// MARK: - Helpers

private func makeDependencies(
    _ harness: Harness,
    installer: StubInstaller? = nil,
    codesign: StubCodesignVerifier = StubCodesignVerifier(),
    signature: any UpdateSignatureChecking = StubSignatureChecker { _, _ in },
    maxPayloadBytes: Int64 = AgentUpdateSession.maxPayloadBytes,
    relaunchThrows: Bool = false
) -> AgentUpdateSession.Dependencies {
    AgentUpdateSession.Dependencies(
        stagingRoot: harness.stagingRoot,
        maxPayloadBytes: maxPayloadBytes,
        currentBundleURL: harness.currentBundle,
        codesign: codesign,
        signature: signature,
        installer: installer ?? StubInstaller(log: harness.log, stagedApp: harness.stagedApp),
        relauncher: SpyRelauncher(log: harness.log, throwOnRelaunch: relaunchThrows)
    )
}

private func runSession(
    _ messages: [Wendy_Agent_Services_V1_UpdateAgentRequest],
    deps: AgentUpdateSession.Dependencies,
    log: SessionLog,
    ackThrows: Bool = false
) async throws {
    try await AgentUpdateSession.run(
        messages: makeStream(messages),
        writeResponse: { response in
            log.record("ack")
            log.append(response)
            if ackThrows { throw StubError.ackFailed }
        },
        deps: deps,
        logger: Logger(label: "test")
    )
}

private func runSessionExpectingFailure(
    _ messages: [Wendy_Agent_Services_V1_UpdateAgentRequest],
    deps: AgentUpdateSession.Dependencies,
    log: SessionLog
) async -> RPCError? {
    do {
        try await runSession(messages, deps: deps, log: log)
        Issue.record("Expected AgentUpdateSession.run to throw")
        return nil
    } catch let error as RPCError {
        return error
    } catch {
        Issue.record("Expected an RPCError, got \(error)")
        return nil
    }
}

private func chunkRequest(_ data: some DataProtocol) -> Wendy_Agent_Services_V1_UpdateAgentRequest {
    var chunk = Wendy_Agent_Services_V1_UpdateAgentRequest.Chunk()
    chunk.data = Data(data)

    var request = Wendy_Agent_Services_V1_UpdateAgentRequest()
    request.requestType = .chunk(chunk)
    return request
}

private func updateRequest(
    sha256: String,
    signature: Data = Data()
) -> Wendy_Agent_Services_V1_UpdateAgentRequest {
    var update = Wendy_Agent_Services_V1_UpdateAgentRequest.ControlCommand.Update()
    update.sha256 = sha256
    update.signature = signature

    var control = Wendy_Agent_Services_V1_UpdateAgentRequest.ControlCommand()
    control.command = .update(update)

    var request = Wendy_Agent_Services_V1_UpdateAgentRequest()
    request.requestType = .control(control)
    return request
}

private func makeStream(
    _ messages: [Wendy_Agent_Services_V1_UpdateAgentRequest]
) -> AsyncStream<Wendy_Agent_Services_V1_UpdateAgentRequest> {
    AsyncStream { continuation in
        for message in messages { continuation.yield(message) }
        continuation.finish()
    }
}

private func makeStreamingRequest(
    _ messages: [Wendy_Agent_Services_V1_UpdateAgentRequest]
) -> StreamingServerRequest<Wendy_Agent_Services_V1_UpdateAgentRequest> {
    StreamingServerRequest(
        metadata: [:],
        messages: RPCAsyncSequence(
            wrapping: AsyncThrowingStream<
                Wendy_Agent_Services_V1_UpdateAgentRequest, any Error
            > { continuation in
                for message in messages { continuation.yield(message) }
                continuation.finish()
            }
        )
    )
}

private func makeUpdateContext() -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyAgentService",
            method: "UpdateAgent"
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}
