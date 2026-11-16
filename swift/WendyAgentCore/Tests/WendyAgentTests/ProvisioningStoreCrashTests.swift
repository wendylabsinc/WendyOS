import Crypto
import Darwin
import Dispatch
import Foundation
import Testing

@testable import WendyAgentCore

@Suite("ProvisioningStore process crashes", .serialized)
struct ProvisioningStoreCrashTests {
    private struct Fixture: Codable, Sendable {
        var mode: String
        var phase: String
        var key: String
        var publicKey: Data
    }

    private struct State: Codable {
        var enrolled = true
        var cloudHost = "fixture.invalid:443"
        var orgId: Int32 = 7
        var assetId: Int32 = 42
        var keyPem: String?
        var certPem = "fixture certificate bytes"
        var chainPem = "fixture chain bytes"
        var principalURI = "urn:wendy:fixture:device:42"
    }

    private func save(_ store: ProvisioningStore, key: String) throws {
        let state = State()
        try store.save(
            cloudHost: state.cloudHost,
            orgID: state.orgId,
            assetID: state.assetId,
            keyBacking: .softwarePEM(key),
            certPEM: state.certPem,
            chainPEM: state.chainPem,
            principalURI: state.principalURI
        )
    }

    @Test("enrollment, repair and legacy migration recover across real process exits")
    func survivesProcessCrash() throws {
        // Only the child entry point runs when launched by this test.
        guard ProcessInfo.processInfo.environment["WENDY_STORE_CRASH_FIXTURE"] == nil else {
            return
        }
        for mode in ["enroll", "repair", "legacy"] {
            for phase in [
                "before:device-key.pem", "after:device-key.pem",
                "before:provisioning.json", "after:provisioning.json",
            ] {
                let root = FileManager.default.temporaryDirectory.appendingPathComponent(
                    "wendy-store-crash-\(UUID().uuidString)"
                )
                defer { try? FileManager.default.removeItem(at: root) }
                let config = root.appendingPathComponent("config")
                try FileManager.default.createDirectory(
                    at: config,
                    withIntermediateDirectories: true
                )
                let key = P256.Signing.PrivateKey()
                let fixture = Fixture(
                    mode: mode,
                    phase: phase,
                    key: key.pemRepresentation,
                    publicKey: key.publicKey.derRepresentation
                )
                let store = ProvisioningStore(configPath: config)
                if mode != "enroll" {
                    try self.save(store, key: fixture.key)
                    if mode == "repair" {
                        for name in ["device-key.pem", "provisioning.json"] {
                            try FileManager.default.setAttributes(
                                [.posixPermissions: 0o644],
                                ofItemAtPath: config.appendingPathComponent(name).path
                            )
                        }
                    } else {
                        var legacy = State()
                        legacy.keyPem = fixture.key
                        try JSONEncoder().encode(legacy).write(
                            to: config.appendingPathComponent("provisioning.json")
                        )
                        try FileManager.default.removeItem(
                            at: config.appendingPathComponent("device-key.pem")
                        )
                    }
                }
                let input = root.appendingPathComponent("fixture.json")
                try JSONEncoder().encode(fixture).write(to: input)
                try FileManager.default.setAttributes(
                    [.posixPermissions: 0o600],
                    ofItemAtPath: input.path
                )
                for action in ["crash", "recover", "verify"] {
                    try self.runChild(
                        input: input,
                        action: action,
                        expected: action == "crash" ? 73 : 0
                    )
                }
            }
        }
    }

    private func runChild(input: URL, action: String, expected: Int32) throws {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: CommandLine.arguments[0])
        // SwiftPM's native macOS runner loads a test bundle through a helper.
        // Other build systems execute the test binary directly.
        var bundleArguments: [String] = []
        if let index = CommandLine.arguments.firstIndex(of: "--test-bundle-path") {
            bundleArguments = ["--test-bundle-path", CommandLine.arguments[index + 1]]
        }
        process.arguments =
            bundleArguments + [
                "--testing-library", "swift-testing", "--filter",
                "ProvisioningStoreCrashTests/child",
            ]
        var environment = ProcessInfo.processInfo.environment
        environment["WENDY_STORE_CRASH_FIXTURE"] = input.path
        environment["WENDY_STORE_CRASH_ACTION"] = action
        process.environment = environment
        let output = input.deletingLastPathComponent().appendingPathComponent("\(action).log")
        #expect(FileManager.default.createFile(atPath: output.path, contents: nil))
        let handle = try FileHandle(forWritingTo: output)
        defer { try? handle.close() }
        process.standardOutput = handle
        process.standardError = handle
        let finished = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in finished.signal() }
        try process.run()
        let result = finished.wait(timeout: .now() + 20)
        if result == .timedOut {
            Darwin.kill(process.processIdentifier, SIGKILL)
            process.waitUntilExit()
        }
        let log = (try? String(contentsOf: output, encoding: .utf8)) ?? "unreadable child log"
        try #require(result == .success, "\(action) timed out: \(log)")
        try #require(process.terminationStatus == expected, "\(action): \(log)")
    }

    @Test("subprocess entry point")
    func child() throws {
        let environment = ProcessInfo.processInfo.environment
        guard let input = environment["WENDY_STORE_CRASH_FIXTURE"] else { return }
        let action = try #require(environment["WENDY_STORE_CRASH_ACTION"])
        let fixture = try JSONDecoder().decode(
            Fixture.self,
            from: Data(contentsOf: URL(fileURLWithPath: input))
        )
        let config = URL(fileURLWithPath: input).deletingLastPathComponent().appendingPathComponent(
            "config"
        )
        let store = ProvisioningStore(
            configPath: config,
            observeWrite: { url, boundary in
                if action == "crash",
                    fixture.phase == "\(boundary.rawValue):\(url.lastPathComponent)"
                {
                    // No defers or shutdown callbacks may repair this state.
                    Darwin._exit(73)
                }
            }
        )
        if action == "crash" {
            if fixture.mode == "enroll" {
                try self.save(store, key: fixture.key)
            } else {
                _ = store.load()
            }
            Issue.record("requested crash boundary was not reached")
            return
        }
        let stateURL = config.appendingPathComponent("provisioning.json")
        let keyURL = config.appendingPathComponent("device-key.pem")
        if FileManager.default.fileExists(atPath: stateURL.path) {
            let state = try JSONDecoder().decode(State.self, from: Data(contentsOf: stateURL))
            let key = try? String(contentsOf: keyURL, encoding: .utf8)
            #expect(
                key == fixture.key || state.keyPem == fixture.key,
                "committed enrollment lost its only key"
            )
        }
        if action == "recover", fixture.mode == "enroll", fixture.phase != "after:provisioning.json"
        {
            #expect(store.load() == nil, "interrupted initial enrollment falsely committed")
            #expect(!FileManager.default.fileExists(atPath: stateURL.path))
            try self.save(store, key: fixture.key)
        }
        let loaded = try #require(store.load())
        let expected = State()
        #expect(loaded.enrolled)
        #expect(loaded.cloudHost == expected.cloudHost)
        #expect(loaded.orgID == expected.orgId)
        #expect(loaded.assetID == expected.assetId)
        #expect(loaded.principalURI == expected.principalURI)
        #expect(loaded.certPEM == expected.certPem)
        #expect(loaded.chainPEM == expected.chainPem)
        #expect(loaded.keyBacking == .softwarePEM(fixture.key))
        let recovered = try P256.Signing.PrivateKey(
            pemRepresentation: String(contentsOf: keyURL, encoding: .utf8)
        )
        let challenge = Data("identity after process restart".utf8)
        let signature = try recovered.signature(for: challenge)
        #expect(
            try P256.Signing.PublicKey(derRepresentation: fixture.publicKey).isValidSignature(
                signature,
                for: challenge
            )
        )
        #expect(
            try JSONDecoder().decode(State.self, from: Data(contentsOf: stateURL)).keyPem == nil
        )
        var before: [URL: NSDictionary] = [:]
        for url in [keyURL, stateURL] {
            let attributes = try FileManager.default.attributesOfItem(atPath: url.path)
            #expect(attributes[.type] as? FileAttributeType == .typeRegular)
            #expect(attributes[.posixPermissions] as? Int == 0o600)
            before[url] = attributes as NSDictionary
        }
        #expect(store.load() != nil)
        for (url, attributes) in before {
            let after = try FileManager.default.attributesOfItem(atPath: url.path)
            for field: FileAttributeKey in [
                .systemFileNumber, .modificationDate, .ownerAccountID, .groupOwnerAccountID,
            ] {
                #expect(after[field] as? NSObject == attributes[field] as? NSObject)
            }
        }
        for name in ["device.pem", "ca.pem", ".provisioned"] {
            #expect(
                !FileManager.default.fileExists(atPath: config.appendingPathComponent(name).path)
            )
        }
    }
}
