import Foundation
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("PersistedRestartPolicy")
struct RestartPolicyTests {
    @Test("proto DEFAULT maps to unless-stopped")
    func protoDefaultMapsToUnlessStopped() {
        var proto = Wendy_Agent_Services_V1_RestartPolicy()
        proto.mode = .default

        let persisted = PersistedRestartPolicy(from: proto)

        #expect(persisted.mode == .unlessStopped)
        #expect(persisted.onFailureMaxRetries == 0)
    }

    @Test("proto UNLESS_STOPPED maps to unless-stopped")
    func protoUnlessStoppedMapsToUnlessStopped() {
        var proto = Wendy_Agent_Services_V1_RestartPolicy()
        proto.mode = .unlessStopped

        let persisted = PersistedRestartPolicy(from: proto)

        #expect(persisted.mode == .unlessStopped)
        #expect(persisted.onFailureMaxRetries == 0)
    }

    @Test("proto NO maps to no")
    func protoNoMapsToNo() {
        var proto = Wendy_Agent_Services_V1_RestartPolicy()
        proto.mode = .no

        let persisted = PersistedRestartPolicy(from: proto)

        #expect(persisted.mode == .no)
        #expect(persisted.onFailureMaxRetries == 0)
    }

    @Test("proto ON_FAILURE maps to onFailure carrying maxRetries")
    func protoOnFailureMapsToOnFailureWithMaxRetries() {
        var proto = Wendy_Agent_Services_V1_RestartPolicy()
        proto.mode = .onFailure
        proto.onFailureMaxRetries = 5

        let persisted = PersistedRestartPolicy(from: proto)

        #expect(persisted.mode == .onFailure)
        #expect(persisted.onFailureMaxRetries == 5)
    }

    @Test("proto UNRECOGNIZED maps to unless-stopped")
    func protoUnrecognizedMapsToUnlessStopped() {
        var proto = Wendy_Agent_Services_V1_RestartPolicy()
        proto.mode = .UNRECOGNIZED(99)

        let persisted = PersistedRestartPolicy(from: proto)

        #expect(persisted.mode == .unlessStopped)
        #expect(persisted.onFailureMaxRetries == 0)
    }

    @Test("legacy info.json without stoppedByUser decodes as stopped by the user")
    func legacyInfoJSONDecodesAsStoppedByUser() throws {
        let apps = try JSONDecoder().decode(
            [WendyApp].self,
            from: Data(legacyInfoJSON(appID: "sh.wendy.tests.Legacy").utf8)
        )
        let app = try #require(apps.first)

        // The missing key IS the legacy marker. Decoding it as `false` would
        // make the first reconcile after an agent update resurrect every app
        // the device ever deployed; see `WendyApp.init(from:)`.
        #expect(app.stoppedByUser == true)
        // The policy default is unaffected: it only decides what happens to an
        // app that is eligible to be restarted in the first place.
        #expect(app.restartPolicy == .default)
        #expect(app.restartPolicy.mode == .unlessStopped)
    }

    @Test("a file written by the current agent round-trips stoppedByUser in both states")
    func currentInfoJSONRoundTripsStoppedByUserBothWays() throws {
        for stoppedByUser in [true, false] {
            let app = WendyApp(
                info: WendyAppInfo(
                    id: "sh.wendy.tests.RoundTripFlag",
                    kind: .native,
                    status: .stopped,
                    pid: nil
                ),
                stoppedByUser: stoppedByUser
            )

            let data = try JSONEncoder().encode(app)

            // The key must always be written, since its absence is what marks a
            // file as legacy.
            let rawObject = try #require(
                try JSONSerialization.jsonObject(with: data) as? [String: Any]
            )
            #expect(rawObject["stoppedByUser"] != nil)

            let decoded = try JSONDecoder().decode(WendyApp.self, from: data)
            #expect(decoded.stoppedByUser == stoppedByUser)
        }
    }

    @Test("round trip preserves policy and stoppedByUser, drops runtime-only fields")
    func roundTripPreservesPersistedFieldsAndDropsRuntimeOnlyFields() throws {
        var app = WendyApp(
            info: WendyAppInfo(
                id: "sh.wendy.tests.RoundTrip",
                kind: .native,
                status: .stopped,
                pid: nil
            ),
            restartPolicy: PersistedRestartPolicy(mode: .onFailure, onFailureMaxRetries: 7),
            stoppedByUser: true
        )
        // Populate the runtime-only fields to prove they don't leak into the
        // persisted representation.
        app.failureCount = 3
        app.lastRestart = Date()
        app.lastExitCode = 42

        let data = try JSONEncoder().encode(app)

        // Assert on the raw JSON, not just the round-tripped struct, so a
        // decoder that silently ignores unknown keys can't mask a leak.
        let rawObject = try #require(
            try JSONSerialization.jsonObject(with: data) as? [String: Any]
        )
        #expect(rawObject["failureCount"] == nil)
        #expect(rawObject["lastRestart"] == nil)
        #expect(rawObject["lastExitCode"] == nil)
        #expect(rawObject["restartPolicy"] != nil)
        #expect(rawObject["stoppedByUser"] != nil)

        let decoded = try JSONDecoder().decode(WendyApp.self, from: data)
        #expect(
            decoded.restartPolicy
                == PersistedRestartPolicy(mode: .onFailure, onFailureMaxRetries: 7)
        )
        #expect(decoded.stoppedByUser == true)
        #expect(decoded.failureCount == 0)
        #expect(decoded.lastRestart == nil)
        #expect(decoded.lastExitCode == nil)
    }
}

/// An `info.json` entry as an agent from before restart parity wrote it: no
/// `restartPolicy`, no `stoppedByUser`.
func legacyInfoJSON(appID: String, binaryName: String = "app") -> String {
    """
    [
        {
            "info": {
                "id": "\(appID)",
                "kind": "native",
                "status": "stopped",
                "pid": null
            },
            "native": {
                "directory": "/tmp/\(appID)",
                "binaryName": "\(binaryName)",
                "args": []
            }
        }
    ]
    """
}
