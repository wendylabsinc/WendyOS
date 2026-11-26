import Foundation
import Testing
import WendyE2ETesting

let appsDedicatedDeviceTestsEnabled =
    appsDeploymentTestsEnabled
    && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_DEDICATED_DEVICE"] == "true"

extension AppsDeploymentFixture {
    enum Service: String, Sendable {
        case agent = "wendy-agent.service"
        case runtime = "containerd.service"
    }

    /// Ordinary service controls on an explicitly dedicated, verified device.
    /// No arbitrary commands or opaque scripts can be supplied by the runner.
    func withStoppedService(
        _ cli: WendyE2ESession,
        _ service: Service,
        _ body: @Sendable () async throws -> Void
    ) async throws {
        try #require(
            appsDedicatedDeviceTestsEnabled,
            "Dedicated device opt-in is required for service control"
        )
        if service == .agent,
            ProcessInfo.processInfo.environment["WENDY_E2E_APPS_SYSTEMD_OUTAGE"] == "true"
        {
            try await withIndependentAgentRestart(cli, body)
            return
        }
        let sshUser = try Self.setting("WENDY_E2E_APPS_SSH_USER")
        let ssh =
            "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5 -l \(Self.quote(sshUser)) \(Self.quote(device))"
        var primaryError: (any Error)?
        do {
            _ = try await succeed(cli, "\(ssh) sudo -n systemctl stop \(service.rawValue)")
            try await body()
        } catch { primaryError = error }
        do {
            _ = try await succeed(cli, "\(ssh) sudo -n systemctl start \(service.rawValue)")
        } catch {
            Issue.record(
                "Dedicated service restoration failed for \(device) \(service.rawValue): \(error)"
            )
            primaryError = primaryError ?? error
        }
        if let primaryError { throw primaryError }
    }
}

let appsRebootTestsEnabled =
    appsDedicatedDeviceTestsEnabled
    && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_REBOOT"] == "true"

extension AppsDeploymentFixture {
    func rebootDedicatedDevice(_ cli: WendyE2ESession) async throws {
        try #require(
            appsRebootTestsEnabled,
            "Full device reboot needs its own explicit fixture opt-in"
        )
        let user = try Self.setting("WENDY_E2E_APPS_SSH_USER")
        let ssh =
            "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5 -l \(Self.quote(user)) \(Self.quote(device))"
        let before = try await succeed(cli, "\(ssh) cat /proc/sys/kernel/random/boot_id")
            .normalizedStdout.trimmingCharacters(in: .whitespacesAndNewlines)
        try #require(UUID(uuidString: before) != nil)
        // The reboot may close SSH before it sends its status. A changed Linux
        // boot id, then verified normal device access, is the required evidence.
        _ = try await command(cli, "\(ssh) sudo -n systemctl reboot")
        let deadline = ContinuousClock.now.advanced(by: .seconds(180))
        repeat {
            let boot = try await command(cli, "\(ssh) cat /proc/sys/kernel/random/boot_id")
            let value = boot.normalizedStdout.trimmingCharacters(in: .whitespacesAndNewlines)
            if boot.status.isSuccess, UUID(uuidString: value) != nil, value != before {
                try await verifyIdentity(cli)
                return
            }
            try await Task.sleep(for: .seconds(3))
        } while ContinuousClock.now < deadline
        throw Prerequisite(
            description:
                "No independently observed full reboot of the dedicated device within 180 seconds"
        )
    }
}

extension AppsDeploymentFixture {
    func bootID(_ cli: WendyE2ESession) async throws -> String {
        try #require(appsDedicatedDeviceTestsEnabled)
        let user = try Self.setting("WENDY_E2E_APPS_SSH_USER")
        let command =
            "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5 -l \(Self.quote(user)) \(Self.quote(device)) cat /proc/sys/kernel/random/boot_id"
        let value = try await succeed(cli, command).normalizedStdout.trimmingCharacters(
            in: .whitespacesAndNewlines
        )
        try #require(UUID(uuidString: value) != nil)
        return value
    }
}

extension AppsDeploymentFixture {
    /// Read-only image records. Client.New uses namespace "default" and
    /// containerd.DefaultAddress is /run/containerd/containerd.sock.
    /// The expected name comes from services.ReleaseImageName in the adapter.
    func imageRecords(_ cli: WendyE2ESession) async throws -> Set<String> {
        try #require(appsDedicatedDeviceTestsEnabled)
        let user = try Self.setting("WENDY_E2E_APPS_SSH_USER")
        let ssh =
            "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5 -l \(Self.quote(user)) \(Self.quote(device))"
        let result = try await succeed(
            cli,
            "\(ssh) sudo -n ctr --address /run/containerd/containerd.sock --namespace default images list --quiet"
        )
        return Set(result.normalizedStdout.split(separator: "\n").map(String.init))
    }

    func requireImageRecordAbsent(_ cli: WendyE2ESession, name: String) async throws -> Set<String>
    {
        let deadline = ContinuousClock.now.advanced(by: .seconds(120))
        repeat {
            let records = try await imageRecords(cli)
            if !records.contains(name) { return records }
            try await Task.sleep(for: .seconds(2))
        } while ContinuousClock.now < deadline
        throw Prerequisite(
            description: "Owned imported image record remains after removal: \(name)"
        )
    }
}
