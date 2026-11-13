import Foundation
import ServiceManagement

// Substitutes ServiceManagement, so no launch agent is registered or removed.
final class TestLaunchAgentService: LaunchAgentService {
    var status: SMAppService.Status
    var statusAfterRegister: SMAppService.Status = .enabled
    var registerError: (any Error)?
    var registerCount = 0
    var unregisterCount = 0

    init(_ status: SMAppService.Status) {
        self.status = status
    }

    func register() throws {
        self.registerCount += 1
        if let registerError = self.registerError { throw registerError }
        self.status = self.statusAfterRegister
    }

    func unregister() async throws {
        self.unregisterCount += 1
        self.status = .notRegistered
    }
}

struct TestError: Error {}

@MainActor
final class Harness {
    let service: TestLaunchAgentService
    let legacyLoginItem: TestLaunchAgentService
    let defaults: UserDefaults
    let suiteName = "LaunchAgentSupervisionTests.\(UUID().uuidString)"
    /// Answers for successive `otherInstanceIsRunning` checks; the last repeats.
    var otherInstanceAnswers: [Bool]
    /// When set, launchd's instance appears once this many registrations happened.
    var instanceAppearsAfterRegistrations: Int?
    var openedLoginItemsSettings = 0
    var relaunchRequests = 0
    private(set) var supervision: LaunchAgentSupervision!

    init(
        isLaunchdInstance: Bool,
        status: SMAppService.Status,
        legacyStatus: SMAppService.Status = .notRegistered,
        preference: Bool? = nil,
        otherInstanceAnswers: [Bool] = [false]
    ) {
        self.service = TestLaunchAgentService(status)
        self.legacyLoginItem = TestLaunchAgentService(legacyStatus)
        self.defaults = UserDefaults(suiteName: self.suiteName)!
        if let preference {
            self.defaults.set(preference, forKey: "launchAtLoginEnabled")
        }
        self.otherInstanceAnswers = otherInstanceAnswers
        self.supervision = LaunchAgentSupervision(
            isLaunchdInstance: isLaunchdInstance,
            service: self.service,
            legacyLoginItem: self.legacyLoginItem,
            defaults: self.defaults,
            otherInstanceIsRunning: { [unowned self] in
                if let registrations = self.instanceAppearsAfterRegistrations {
                    return self.service.registerCount >= registrations
                }
                return self.otherInstanceAnswers.count > 1
                    ? self.otherInstanceAnswers.removeFirst() : self.otherInstanceAnswers[0]
            },
            openLoginItemsSettings: { [unowned self] in self.openedLoginItemsSettings += 1 },
            registrationRetryInterval: .milliseconds(200),
            handOffTimeout: .milliseconds(500),
            otherInstanceExitTimeout: .milliseconds(500)
        )
        self.supervision.onRelaunchRequired = { [unowned self] in self.relaunchRequests += 1 }
    }

    deinit {
        UserDefaults.standard.removePersistentDomain(forName: self.suiteName)
    }
}

@MainActor
func expect(
    _ harness: Harness,
    _ decision: LaunchAgentSupervision.LaunchDecision,
    line: UInt = #line
) async {
    let actual = await harness.supervision.prepareForLaunch()
    precondition(actual == decision, "expected \(decision), got \(actual)", line: line)
}

@main
struct LaunchAgentSupervisionTests {
    @MainActor
    static func main() async {
        // launchd's instance runs the agent and retires the legacy login item.
        var h = Harness(isLaunchdInstance: true, status: .enabled, legacyStatus: .enabled)
        await expect(h, .run)
        precondition(h.legacyLoginItem.unregisterCount == 1)
        precondition(h.service.registerCount == 0 && h.service.unregisterCount == 0)
        precondition(h.supervision.isEnabled)

        // launchd's instance cannot unregister itself; it relaunches instead.
        h = Harness(isLaunchdInstance: true, status: .enabled, preference: false)
        await expect(h, .relaunch)
        precondition(h.service.unregisterCount == 0)

        // launchd's instance waits for a handing-off launch to quit.
        h = Harness(
            isLaunchdInstance: true,
            status: .enabled,
            otherInstanceAnswers: [true, true, false]
        )
        await expect(h, .run)

        // ...but leaves the agent to an unsupervised instance that keeps running.
        h = Harness(isLaunchdInstance: true, status: .enabled, otherInstanceAnswers: [true])
        await expect(h, .quit)

        // Turned off: unregister and run without supervision.
        h = Harness(isLaunchdInstance: false, status: .enabled, preference: false)
        await expect(h, .run)
        precondition(h.service.unregisterCount == 1 && h.service.registerCount == 0)
        precondition(!h.supervision.isEnabled)

        // launchd's instance already runs the agent: quit, registration untouched.
        h = Harness(isLaunchdInstance: false, status: .enabled, otherInstanceAnswers: [true])
        await expect(h, .quit)
        precondition(h.service.registerCount == 0 && h.service.unregisterCount == 0)

        // Fresh install or update: register afresh, hand off once launchd's instance appears.
        h = Harness(
            isLaunchdInstance: false,
            status: .enabled,
            otherInstanceAnswers: [false, false, true]
        )
        await expect(h, .quit)
        precondition(h.service.unregisterCount == 1 && h.service.registerCount == 1)

        h = Harness(
            isLaunchdInstance: false,
            status: .notRegistered,
            otherInstanceAnswers: [false, true]
        )
        await expect(h, .quit)
        precondition(h.service.unregisterCount == 0 && h.service.registerCount == 1)

        // The first registration after an update can fail to spawn; register again.
        h = Harness(isLaunchdInstance: false, status: .enabled)
        h.instanceAppearsAfterRegistrations = 2
        await expect(h, .quit)
        precondition(h.service.unregisterCount == 2 && h.service.registerCount == 2)

        // Turned off in System Settings: respect it and run unsupervised.
        h = Harness(isLaunchdInstance: false, status: .requiresApproval)
        await expect(h, .run)
        precondition(h.service.registerCount == 0 && h.service.unregisterCount == 0)

        // Registration fails: run unsupervised.
        h = Harness(isLaunchdInstance: false, status: .notRegistered)
        h.service.registerError = TestError()
        await expect(h, .run)

        // launchd never starts the agent: keep registering until the hand-off times out.
        h = Harness(isLaunchdInstance: false, status: .notRegistered)
        await expect(h, .run)
        precondition(h.service.registerCount >= 2)

        // Toggle on in an unsupervised instance relaunches it into launchd.
        h = Harness(isLaunchdInstance: false, status: .notRegistered, preference: false)
        h.supervision.setEnabled(true)
        precondition(h.relaunchRequests == 1 && h.supervision.preference)

        // Toggle off in launchd's instance relaunches it outside launchd.
        h = Harness(isLaunchdInstance: true, status: .enabled)
        h.supervision.setEnabled(false)
        precondition(h.relaunchRequests == 1 && !h.supervision.preference)

        // Toggle off in an unsupervised instance only unregisters.
        h = Harness(isLaunchdInstance: false, status: .enabled)
        h.supervision.setEnabled(false)
        for _ in 0..<10 where h.service.unregisterCount == 0 { await Task.yield() }
        precondition(h.relaunchRequests == 0 && h.service.unregisterCount == 1)

        // Toggle on while turned off in System Settings sends the user there.
        h = Harness(isLaunchdInstance: false, status: .requiresApproval, preference: false)
        h.supervision.setEnabled(true)
        precondition(h.openedLoginItemsSettings == 1 && h.relaunchRequests == 0)

        print("LaunchAgentSupervisionTests passed")
    }
}
