import AppKit
import OSLog
import ServiceManagement

/// The part of `SMAppService` that launch agent supervision uses.
protocol LaunchAgentService: AnyObject {
    var status: SMAppService.Status { get }
    func register() throws
    func unregister() async throws
}

extension SMAppService: LaunchAgentService {}

/// Keeps Wendy Agent running as a user launch agent. launchd starts it at
/// login and restarts it after a crash; Quit and self-update exit cleanly, so
/// they stay stopped until the next login or launch.
///
/// While the setting is on, only the instance launchd started runs the agent.
/// Any other launch (Finder, Homebrew, the update relauncher) refreshes the
/// registration, waits for launchd's instance to appear and quits. If
/// registration is unavailable, it runs the agent itself as before.
final class LaunchAgentSupervision {
    enum LaunchDecision: Equatable {
        /// Run the agent in this process.
        case run
        /// Another instance runs the agent; quit without starting it.
        case quit
        /// This instance cannot apply the setting itself; relaunch outside
        /// launchd so the new instance can.
        case relaunch
    }

    static let plistName = "sh.wendy.WendyAgentMac.plist"
    /// Set only by the launch agent plist.
    static let launchdEnvironmentKey = "WENDY_AGENT_LAUNCHD"
    private static let preferenceKey = "launchAtLoginEnabled"
    private static let pollInterval = Duration.milliseconds(200)

    let isLaunchdInstance: Bool
    /// Called when applying a changed setting requires the relaunch described
    /// by `LaunchDecision.relaunch`.
    var onRelaunchRequired: (() -> Void)?

    private let service: any LaunchAgentService
    private let legacyLoginItem: any LaunchAgentService
    private let defaults: UserDefaults
    private let otherInstanceIsRunning: () -> Bool
    private let openLoginItemsSettings: () -> Void
    /// How long a handing-off launch waits for launchd's instance to appear
    /// before registering again.
    private let registrationRetryInterval: Duration
    /// How long a handing-off launch keeps trying before it runs the agent itself.
    private let handOffTimeout: Duration
    /// How long launchd's instance waits for a handing-off launch to quit.
    private let otherInstanceExitTimeout: Duration
    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier ?? "sh.wendy.WendyAgentMac",
        category: "LaunchAgentSupervision"
    )

    convenience init() {
        let environment = ProcessInfo.processInfo.environment
        // Keep the marker away from the update relauncher and anything else
        // this process spawns.
        unsetenv(Self.launchdEnvironmentKey)

        self.init(
            isLaunchdInstance: environment[Self.launchdEnvironmentKey] == "1",
            service: SMAppService.agent(plistName: Self.plistName),
            legacyLoginItem: SMAppService.mainApp,
            defaults: .standard,
            otherInstanceIsRunning: Self.otherInstanceOfThisAppIsRunning,
            openLoginItemsSettings: SMAppService.openSystemSettingsLoginItems
        )
    }

    init(
        isLaunchdInstance: Bool,
        service: any LaunchAgentService,
        legacyLoginItem: any LaunchAgentService,
        defaults: UserDefaults,
        otherInstanceIsRunning: @escaping () -> Bool,
        openLoginItemsSettings: @escaping () -> Void,
        registrationRetryInterval: Duration = .seconds(15),
        handOffTimeout: Duration = .seconds(50),
        otherInstanceExitTimeout: Duration = .seconds(30)
    ) {
        self.isLaunchdInstance = isLaunchdInstance
        self.service = service
        self.legacyLoginItem = legacyLoginItem
        self.defaults = defaults
        self.otherInstanceIsRunning = otherInstanceIsRunning
        self.openLoginItemsSettings = openLoginItemsSettings
        self.registrationRetryInterval = registrationRetryInterval
        self.handOffTimeout = handOffTimeout
        self.otherInstanceExitTimeout = otherInstanceExitTimeout
    }

    /// The user's choice; on unless turned off.
    var preference: Bool {
        self.defaults.object(forKey: Self.preferenceKey) == nil
            || self.defaults.bool(forKey: Self.preferenceKey)
    }

    var isEnabled: Bool {
        self.preference && self.service.status == .enabled
    }

    func prepareForLaunch() async -> LaunchDecision {
        self.logger.info(
            "Launching \(self.isLaunchdInstance ? "under launchd" : "outside launchd", privacy: .public); launch agent status: \(self.service.status.logDescription, privacy: .public)"
        )
        await self.retireLegacyLoginItem()

        if self.isLaunchdInstance {
            // launchd's instance cannot unregister its own job without being
            // killed, so an unsupervised instance has to do it.
            guard self.preference else { return .relaunch }

            // A launch that handed off to this instance may still be quitting.
            if await self.wait(
                for: { !self.otherInstanceIsRunning() },
                timeout: self.otherInstanceExitTimeout
            ) {
                return .run
            }
            // An unsupervised instance kept running the agent. Exit cleanly
            // rather than run a second agent; launchd does not restart a clean
            // exit.
            self.logger.warning(
                "Another Wendy Agent instance is still running; leaving the agent to it"
            )
            return .quit
        }

        guard self.preference else {
            await self.unregister()
            return .run
        }

        // launchd's instance already runs the agent.
        if self.otherInstanceIsRunning() { return .quit }

        // After the bundle is replaced (an update), launchd can fail to spawn
        // the re-registered agent (EX_CONFIG). Observed on macOS 27: a later
        // registration succeeds once about ten seconds have passed without
        // one, so retry slowly rather than often.
        let clock = ContinuousClock()
        let deadline = clock.now.advanced(by: self.handOffTimeout)
        var attempt = 0
        repeat {
            attempt += 1
            guard await self.refreshRegistration() == .enabled else { return .run }

            if await self.wait(
                for: self.otherInstanceIsRunning,
                timeout: self.registrationRetryInterval
            ) {
                self.logger.info(
                    "Handed Wendy Agent over to launchd after \(attempt, privacy: .public) registration(s)"
                )
                return .quit
            }
            self.logger.warning(
                "launchd has not started Wendy Agent after registration \(attempt, privacy: .public)"
            )
        } while clock.now < deadline
        self.logger.error("launchd did not start Wendy Agent; running it without supervision")
        return .run
    }

    func setEnabled(_ enabled: Bool) {
        self.defaults.set(enabled, forKey: Self.preferenceKey)

        if enabled && self.service.status == .requiresApproval {
            // The user turned Wendy Agent off in System Settings; only they
            // can turn it back on there.
            self.openLoginItemsSettings()
            return
        }

        if enabled == self.isLaunchdInstance {
            // Already running the way the setting asks; just make sure an
            // unsupervised agent is not registered to start at login.
            if !enabled {
                Task { await self.unregister() }
            }
        } else {
            // Switching between supervised and unsupervised needs a fresh
            // instance; see `prepareForLaunch()`.
            self.onRelaunchRequired?()
        }
    }

    /// Registers the launch agent afresh: Apple requires re-registration after
    /// the plist or executable changes, which every app update does.
    private func refreshRegistration() async -> SMAppService.Status {
        do {
            switch self.service.status {
            case .requiresApproval:
                self.logger.notice(
                    "Wendy Agent is turned off in System Settings > General > Login Items & Extensions"
                )
                return .requiresApproval
            case .enabled:
                try await self.service.unregister()
            case .notRegistered, .notFound:
                break
            @unknown default:
                break
            }
            try self.service.register()
        } catch {
            self.logger.error(
                "Failed to register the Wendy Agent launch agent: \(String(describing: error), privacy: .public)"
            )
        }
        let status = self.service.status
        self.logger.info(
            "Launch agent status after registration: \(status.logDescription, privacy: .public)"
        )
        return status
    }

    private func unregister() async {
        guard self.service.status != .notRegistered else { return }

        do {
            try await self.service.unregister()
            self.logger.info("Unregistered the Wendy Agent launch agent")
        } catch {
            self.logger.error(
                "Failed to unregister the Wendy Agent launch agent: \(String(describing: error), privacy: .public)"
            )
        }
    }

    /// Earlier versions registered the app itself as a login item. Unregistering
    /// it leaves the running app alone.
    private func retireLegacyLoginItem() async {
        switch self.legacyLoginItem.status {
        case .enabled, .requiresApproval:
            do {
                try await self.legacyLoginItem.unregister()
                self.logger.info("Removed the legacy Wendy Agent login item")
            } catch {
                self.logger.error(
                    "Failed to remove the legacy Wendy Agent login item: \(String(describing: error), privacy: .public)"
                )
            }
        case .notRegistered, .notFound:
            break
        @unknown default:
            break
        }
    }

    private func wait(for condition: () -> Bool, timeout: Duration) async -> Bool {
        let clock = ContinuousClock()
        let deadline = clock.now.advanced(by: timeout)
        while !condition() {
            guard clock.now < deadline else { return false }
            try? await Task.sleep(for: Self.pollInterval)
        }
        return true
    }

    private static func otherInstanceOfThisAppIsRunning() -> Bool {
        guard let bundleIdentifier = Bundle.main.bundleIdentifier else { return false }

        let currentPID = ProcessInfo.processInfo.processIdentifier
        return NSRunningApplication.runningApplications(withBundleIdentifier: bundleIdentifier)
            .contains { $0.processIdentifier != currentPID && !$0.isTerminated }
    }
}

extension SMAppService.Status {
    fileprivate var logDescription: String {
        switch self {
        case .notRegistered: "not registered"
        case .enabled: "enabled"
        case .requiresApproval: "requires approval"
        case .notFound: "not found"
        @unknown default: "unknown (\(self.rawValue))"
        }
    }
}
