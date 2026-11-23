import Foundation
import Testing
import WendyE2ETesting

let appsFleetTestsEnabled =
    appsDeploymentTestsEnabled
    && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FLEET"] == "true"

struct AppsFleetDevice: Decodable, Sendable {
    let asset: String
    let host: String
}
struct AppsFleetLease: Decodable, Sendable {
    let site: String
    let tag: String
}

extension AppsDeploymentFixture {
    func target(_ device: AppsFleetDevice, name: String? = nil) throws -> Self {
        try Self(
            deviceAddress: device.host,
            assetID: device.asset,
            appID: app,
            attemptID: attempt,
            deploymentName: name ?? self.name,
            portNumber: port
        )
    }

    static func withFleet(
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body:
            @Sendable (WendyE2ESession, Self, [AppsFleetDevice], AppsFleetLease) async throws ->
            Void
    ) async throws {
        let path = try setting("WENDY_E2E_APPS_FLEET_PATH")
        let devices = try JSONDecoder().decode(
            [AppsFleetDevice].self,
            from: Data(contentsOf: URL(fileURLWithPath: path))
        )
        try #require(devices.count >= 3, "Declare A, B and an unrelated C in the dedicated fleet")
        try #require(Set(devices.map(\.asset)).count == devices.count)
        try await withApp(filePath: filePath, function: function, line: line) { cli, app in
            try #require(devices[0].asset == app.asset && devices[0].host == app.device)
            // Verify every endpoint/tenant/asset pin before any site or tag write.
            for device in devices {
                let target = try app.target(device)
                try target.requireDedicatedIdentityFixture()
                try await target.verifyIdentity(cli)
            }
            var primaryError: (any Error)?
            do {
                let lease = try await app.observe(cli, "fleet-prepare", as: AppsFleetLease.self)
                try await body(cli, app, devices, lease)
            } catch { primaryError = error }
            // Every declared target participates in owned-app cleanup, including
            // B's detached survivor when an assertion or restart fails midway.
            // Control C is read, never assigned or relabeled by this cleanup.
            for device in devices.dropFirst() {
                do { try await app.target(device).cleanUp(cli) } catch {
                    Issue.record(
                        "Owned app cleanup failed on declared asset \(device.asset): \(error)"
                    )
                    primaryError = primaryError ?? error
                }
            }
            do {
                let exists = try await cli.sh("test -f fleet-lease.json") { $0.status.isSuccess }
                if exists { _ = try await app.succeed(cli, app.observerCommand("fleet-restore")) }
            } catch {
                Issue.record(
                    "Dedicated fleet restoration failed; retain fleet-lease.json: \(error)"
                )
                primaryError = primaryError ?? error
            }
            if let primaryError { throw primaryError }
        }
    }
}
