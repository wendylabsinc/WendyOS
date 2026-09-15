import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

/// A Linux backend stub with a scriptable `listContainers()` result, so tests
/// can drive adoption/restart decisions without a real container runtime.
actor StubLinuxBackend: LinuxContainerBackend {
    private var listing: [LinuxContainerInfo]
    private(set) var pulled: [String] = []
    private(set) var started: [String] = []
    private(set) var stopped: [String] = []
    private(set) var removed: [String] = []
    private var processes: [String: Foundation.Process] = [:]

    /// Which pull (1-based) should hang until `releaseBlockedPull()`. Used to
    /// hold a launch open inside `pull` — the longest await in the container
    /// launch path — so a test can run a supervisor tick in that window.
    /// Deliberately blocks only that one pull, so a second (buggy) launch
    /// proceeds and the test fails on the duplicate rather than deadlocking.
    private var blockedPullNumber: Int?
    private var blockedPullContinuation: CheckedContinuation<Void, Never>?

    init(listing: [LinuxContainerInfo] = []) {
        self.listing = listing
    }

    func setListing(_ listing: [LinuxContainerInfo]) {
        self.listing = listing
    }

    func blockPull(number: Int) {
        self.blockedPullNumber = number
    }

    func releaseBlockedPull() {
        self.blockedPullNumber = nil
        self.blockedPullContinuation?.resume()
        self.blockedPullContinuation = nil
    }

    func pullCount() -> Int { self.pulled.count }

    func pull(image: String) async throws {
        self.pulled.append(image)
        guard self.pulled.count == self.blockedPullNumber else { return }
        await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
            self.blockedPullContinuation = continuation
        }
    }

    func createAndStart(
        appName: String,
        imageName: String,
        appConfig: WendyAppConfig?,
        terminationHandler: (@Sendable (Foundation.Process) -> Void)?
    ) async throws -> (process: Foundation.Process, stdout: Pipe, stderr: Pipe) {
        self.started.append(appName)
        // Long-lived so the app stays observably `.running` until the test ends
        // it, mirroring a real attached container run.
        let process = Foundation.Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sleep")
        process.arguments = ["30"]
        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr
        process.terminationHandler = terminationHandler
        try process.run()
        self.processes[appName] = process
        return (process, stdout, stderr)
    }

    func stop(appName: String) async throws {
        self.stopped.append(appName)
        self.processes[appName]?.terminate()
    }

    func remove(appName: String) async throws {
        self.removed.append(appName)
        self.processes[appName]?.terminate()
        self.processes[appName] = nil
    }

    func listContainers() async throws -> [LinuxContainerInfo] { self.listing }

    func startedApps() -> [String] { self.started }
    func stoppedApps() -> [String] { self.stopped }

    func terminateAll() {
        for process in self.processes.values where process.isRunning {
            process.terminate()
        }
    }
}

/// Records the signals a test's `ContainerService` sends, and answers pid
/// executable-path lookups from a scripted table, so native-survivor handling
/// is exercised without spawning real processes.
final class PIDStub: @unchecked Sendable {
    private let queue = DispatchQueue(label: "wendy.tests.pid-stub")
    private var paths: [Int32: String]
    private var births: [Int32: UInt64] = [:]
    private var signals: [(pid: Int32, signal: Int32)] = []
    /// When true, a pid is dropped from `paths` the moment it is signalled, so
    /// the survivor wait returns immediately instead of polling for real time.
    private let exitsOnSignal: Bool

    init(paths: [Int32: String] = [:], exitsOnSignal: Bool = true) {
        self.paths = paths
        self.exitsOnSignal = exitsOnSignal
    }

    var lookup: PIDExecutablePathLookup {
        { [self] pid in queue.sync { paths[pid] } }
    }

    var birthLookup: PIDBirthTimeLookup {
        { [self] pid in queue.sync { paths[pid] == nil ? nil : births[pid] } }
    }

    func restoreBirths(_ apps: [WendyApp]) {
        queue.sync {
            for app in apps {
                guard let pid = app.info.pid, let birth = app.pidBirthTime, let path = paths[pid]
                else { continue }
                if births[pid] == nil {
                    births[pid] =
                        path == app.native?.executablePath || path == "/bin/sh" ? birth : birth + 1
                }
            }
        }
    }

    func setBirth(_ pid: Int32, _ birth: UInt64) {
        queue.sync { births[pid] = birth }
    }

    var send: PIDSignalSender {
        { [self] pid, signal in
            queue.sync {
                signals.append((pid, signal))
                if exitsOnSignal { paths[pid] = nil }
            }
        }
    }

    func sentSignals() -> [(pid: Int32, signal: Int32)] {
        queue.sync { signals }
    }
}

@Suite("ContainerService reconcile & supervisor")
struct ContainerServiceSupervisorTests {

    // MARK: - Reconcile

    @Test("reconcile starts an app with the default restart policy")
    func reconcileStartsDefaultPolicyApp() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.ReconcileStarts"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")

        // A fresh service over the same state directory is what an agent
        // restart looks like: everything loads back as stopped.
        let restarted = makeService(appsBase: appsBase)
        #expect(await restarted.appInfo(forAppID: appID)?.status == .stopped)

        await restarted.reconcileApps()

        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)
        await restarted.stopAllApps()
    }

    @Test("reconcile leaves a user-stopped app down")
    func reconcileSkipsUserStoppedApp() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.ReconcileUserStopped"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startNativeApp(service: service, appID: appID)
        try await stopNativeApp(service: service, appID: appID)

        let restarted = makeService(appsBase: appsBase)
        await restarted.reconcileApps()

        #expect(await restarted.appInfo(forAppID: appID)?.status == .stopped)
    }

    @Test("reconcile leaves an app deployed with --no-restart down")
    func reconcileSkipsNoRestartPolicy() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.ReconcileNoRestart"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        var policy = RestartPolicy()
        policy.mode = .no
        try await createNativeApp(
            service: service,
            appID: appID,
            cmd: "sleep.sh",
            restartPolicy: policy
        )

        let restarted = makeService(appsBase: appsBase)
        await restarted.reconcileApps()

        #expect(await restarted.appInfo(forAppID: appID)?.status == .stopped)
    }

    @Test("reconcile never relaunches an app that is already running")
    func reconcileLeavesRunningAppAlone() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.ReconcileAlreadyRunning"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startNativeApp(service: service, appID: appID)
        let runningPID = try #require(await service.appInfo(forAppID: appID)?.pid)

        // Reconcile racing a start that already won would orphan the live
        // process behind a second copy.
        await service.reconcileApps()

        #expect(await service.appInfo(forAppID: appID)?.pid == runningPID)
        await service.stopAllApps()
    }

    @Test("reconcile leaves apps from a legacy info.json down")
    func reconcileSkipsLegacyApps() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        // Exactly what a device carries after upgrading from an agent that
        // predates restart parity: apps persisted with neither `restartPolicy`
        // nor `stoppedByUser`. Without the missing-key-means-stopped rule the
        // first reconcile would start all of them, unbidden.
        let appID = "sh.wendy.tests.LegacyOnDisk"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")
        try writeLegacyInfoJSON(appsBase: appsBase, appID: appID, binaryName: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        await service.reconcileApps()

        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
    }

    @Test("an explicit start makes a legacy app eligible for restarts from then on")
    func explicitStartClearsTheLegacyStopFlag() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.LegacyThenStarted"
        try writeSupervisorCrashScript(appsBase: appsBase, appID: appID, name: "crash.sh", code: 4)
        try writeLegacyInfoJSON(appsBase: appsBase, appID: appID, binaryName: "crash.sh")

        let service = makeService(appsBase: appsBase, restartFloor: .zero)

        // The user starts it: that is explicit intent, and clears the legacy
        // flag.
        try await startNativeApp(service: service, appID: appID)
        try await waitForSupervisor("the legacy app to crash") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }

        // From here on it takes part in restart parity like any other app.
        await service.superviseApps()
        #expect(await service.appInfo(forAppID: appID)?.status == .running)
        #expect(await service.failureCount(forAppID: appID) == 1)

        await service.stopAllApps()

        // And the cleared flag is durable: a later reconcile brings it back.
        let restarted = makeService(appsBase: appsBase)
        await restarted.reconcileApps()
        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)
        await restarted.stopAllApps()
    }

    @Test("reconcile adopts a container the backend reports running")
    func reconcileAdoptsRunningContainer() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "svc-adopt"
        let backend = StubLinuxBackend()
        let service = makeService(appsBase: appsBase, backend: backend)
        try await createLinuxApp(service: service, appID: appID)

        await backend.setListing([
            LinuxContainerInfo(
                id: managedContainerName(for: appID),
                name: managedContainerName(for: appID),
                state: "running"
            )
        ])

        let restarted = makeService(appsBase: appsBase, backend: backend)
        await restarted.reconcileApps()

        // Adopted, not restarted: no launch was issued through the backend.
        #expect(await backend.startedApps().isEmpty)
        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)
        // Adopted apps have no attached process, hence no pid to report.
        #expect(await restarted.appInfo(forAppID: appID)?.pid == nil)
    }

    @Test("reconcile starts a container the backend reports exited")
    func reconcileStartsExitedContainer() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "svc-exited"
        let backend = StubLinuxBackend(listing: [
            LinuxContainerInfo(
                id: managedContainerName(for: appID),
                name: managedContainerName(for: appID),
                state: "exited"
            )
        ])
        let service = makeService(appsBase: appsBase, backend: backend)
        try await createLinuxApp(service: service, appID: appID)

        let restarted = makeService(appsBase: appsBase, backend: backend)
        await restarted.reconcileApps()

        #expect(await backend.startedApps() == [appID])
        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)

        await restarted.stopAllApps()
        await backend.terminateAll()
    }

    @Test("reconcile starts a container the backend does not know about")
    func reconcileStartsMissingContainer() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "svc-missing"
        let backend = StubLinuxBackend(listing: [])
        let service = makeService(appsBase: appsBase, backend: backend)
        try await createLinuxApp(service: service, appID: appID)

        let restarted = makeService(appsBase: appsBase, backend: backend)
        await restarted.reconcileApps()

        #expect(await backend.startedApps() == [appID])

        await restarted.stopAllApps()
        await backend.terminateAll()
    }

    @Test("an adopted container can be stopped through the backend")
    func adoptedContainerCanBeStopped() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "svc-stop-adopted"
        let backend = StubLinuxBackend()
        let service = makeService(appsBase: appsBase, backend: backend)
        try await createLinuxApp(service: service, appID: appID)

        await backend.setListing([
            LinuxContainerInfo(
                id: managedContainerName(for: appID),
                name: managedContainerName(for: appID),
                state: "running"
            )
        ])

        let restarted = makeService(appsBase: appsBase, backend: backend)
        await restarted.reconcileApps()
        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)

        var stopRequest = Wendy_Agent_Services_V1_StopContainerRequest()
        stopRequest.appName = appID
        _ = try await restarted.stopContainer(
            request: ServerRequest(metadata: [:], message: stopRequest),
            context: makeSupervisorServerContext(method: "StopContainer")
        )

        #expect(await backend.stoppedApps() == [appID])
        #expect(await restarted.appInfo(forAppID: appID)?.status == .stopped)
    }

    // MARK: - Supervisor

    @Test("supervisor restarts a crashed app, counts the failure, and honors the restart floor")
    func supervisorRestartsCrashedAppAndHonorsFloor() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.SupervisorRestart"
        try writeSupervisorCrashScript(appsBase: appsBase, appID: appID, name: "crash.sh", code: 3)

        // A real 10 s floor: the second tick must be blocked by it.
        let service = makeService(appsBase: appsBase, restartFloor: .seconds(10))
        try await createNativeApp(service: service, appID: appID, cmd: "crash.sh")
        try await startNativeApp(service: service, appID: appID)

        try await waitForSupervisor("app crashes on its own") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }
        #expect(await service.failureCount(forAppID: appID) == 0)

        await service.superviseApps()

        #expect(await service.appInfo(forAppID: appID)?.status == .running)
        #expect(await service.failureCount(forAppID: appID) == 1)

        try await waitForSupervisor("restarted app crashes again") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }

        // Inside the 10 s floor: the app stays down and the counter doesn't move.
        await service.superviseApps()
        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
        #expect(await service.failureCount(forAppID: appID) == 1)
    }

    @Test("a tick during a container pull does not start a second copy")
    func supervisorSkipsAppLaunchingThroughAPull() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "svc-pull-window"
        let backend = StubLinuxBackend()
        // Hold the launch inside `pull`: token claimed, not yet running. This is
        // the longest await in the container launch path and the actor is free
        // throughout it, so a tick lands here on any real deploy whose pull
        // outlasts one interval.
        await backend.blockPull(number: 1)

        // No floor, so nothing but the in-flight-launch guard can hold the tick
        // back — a fresh StartContainer never stamps `lastRestart`.
        let service = makeService(appsBase: appsBase, backend: backend, restartFloor: .zero)
        try await createLinuxApp(service: service, appID: appID)

        let starter = Task { try await startNativeApp(service: service, appID: appID) }
        try await waitForSupervisor("the pull to begin") {
            await backend.pullCount() >= 1
        }

        await service.superviseApps()

        await backend.releaseBlockedPull()
        try await starter.value
        #expect(await service.appInfo(forAppID: appID)?.status == .running)

        // Exactly one launch: the tick must not have pulled or started again.
        #expect(await backend.startedApps() == [appID])
        #expect(await backend.pullCount() == 1)

        await service.stopAllApps()
        await backend.terminateAll()
    }

    @Test(
        "beginStopping during an in-flight tick's pull stops that tick from launching the next app"
    )
    func beginStoppingDuringInFlightPullStopsSameTickFromLaunchingNextApp() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        // Sorted so `appA` is the candidate `superviseApps`' `keys.sorted()`
        // loop reaches first (and blocks in), and `appB` is the next candidate
        // in that SAME tick.
        let appA = "svc-quiesce-a"
        let appB = "svc-quiesce-b"
        let backend = StubLinuxBackend()
        // Blocks the first pull call the backend sees, i.e. appA's.
        await backend.blockPull(number: 1)

        let service = makeService(appsBase: appsBase, backend: backend, restartFloor: .zero)
        try await createLinuxApp(service: service, appID: appA)
        try await createLinuxApp(service: service, appID: appB)

        let tick = Task { await service.superviseApps() }
        try await waitForSupervisor("appA's pull to begin") {
            await backend.pullCount() >= 1
        }

        // This is the ContainerService-level guarantee the fixed
        // `handleUnexpectedRuntimeExit` (WendyAgent.swift) now relies on:
        // mark the service stopping while a tick is mid-flight, exactly like
        // `stop()` already did before this fix extended the same ordering to
        // the unexpected-runtime-exit path.
        await service.beginStopping()

        await backend.releaseBlockedPull()
        await tick.value

        // appA's already-in-flight launch cannot be cancelled — a pull is not
        // cancellation-aware — so it is allowed to finish, but the same tick's
        // loop must not go on to start appB once it observes `isStopping`.
        #expect(await backend.pullCount() == 1)
        #expect(await backend.startedApps() == [appA])
        #expect(await service.appInfo(forAppID: appB)?.status == .stopped)

        await service.stopAllApps()
        await backend.terminateAll()
    }

    @Test("supervisor ticks never signal a persisted pid")
    func supervisorTickNeverSignalsPersistedPIDs() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.TickNoSignal"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        // Survivor termination belongs to reconcile alone. This is what makes it
        // safe for a warm restart to supervise without reconciling.
        let binaryPath = "\(appsBase)/\(appID)/sleep.sh"
        let pids = PIDStub(paths: [persistedPID: binaryPath])
        let restarted = makeService(appsBase: appsBase, restartFloor: .zero, pids: pids)

        await restarted.superviseApps()

        #expect(pids.sentSignals().isEmpty)
        await restarted.stopAllApps()
    }

    @Test("the supervisor task reconciles on a cold start and not on a warm one")
    func supervisorTaskReconcilesOnlyOnColdStart() async throws {
        // A provisioning transition rebuilds the ContainerService over the same
        // state directory while the previous one's app processes are still
        // running, so its `info.json` pids are NOT survivors. Reconciling there
        // would SIGTERM every running native app.
        let warmBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(warmBase) }
        let coldBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(coldBase) }

        let appID = "sh.wendy.tests.ColdVsWarm"
        try writeSupervisorSleepScript(appsBase: warmBase, appID: appID, name: "sleep.sh")
        try writeSupervisorSleepScript(appsBase: coldBase, appID: appID, name: "sleep.sh")

        let warmPIDs = try await stageSurvivor(appsBase: warmBase, appID: appID)
        let coldPIDs = try await stageSurvivor(appsBase: coldBase, appID: appID)

        // A long interval keeps the periodic tick out of the way; only the
        // reconcile pass can act within the test.
        let warmService = makeService(
            appsBase: warmBase,
            supervisorInterval: .seconds(600),
            pids: warmPIDs
        )
        let warmTask = WendyAgent.makeAppSupervisorTask(
            containerService: warmService,
            reconcile: false
        )
        defer { warmTask.cancel() }

        let coldService = makeService(
            appsBase: coldBase,
            supervisorInterval: .seconds(600),
            pids: coldPIDs
        )
        let coldTask = WendyAgent.makeAppSupervisorTask(
            containerService: coldService,
            reconcile: true
        )
        defer { coldTask.cancel() }

        // The cold task reconciles: survivor terminated, app brought back.
        try await waitForSupervisor("the cold start to reconcile") {
            await coldService.appInfo(forAppID: appID)?.status == .running
        }
        #expect(coldPIDs.sentSignals().map(\.signal) == [SIGTERM])

        // The warm task supervises only: it left the running app alone.
        #expect(warmPIDs.sentSignals().isEmpty)
        #expect(await warmService.appInfo(forAppID: appID)?.status == .stopped)

        await coldService.stopAllApps()
    }

    @Test("a real tick on a warm restart adopts a live native app instead of relaunching it")
    func warmRestartTickAdoptsLiveNativeApp() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.WarmAdopt"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        // The service that owns the running process, standing in for the one a
        // provisioning switch discards. Its app is left running on purpose.
        let owner = makeService(appsBase: appsBase)
        try await createNativeApp(service: owner, appID: appID, cmd: "sleep.sh")
        try await startNativeApp(service: owner, appID: appID)
        let livePID = try #require(await owner.appInfo(forAppID: appID)?.pid)
        defer { Task { await owner.stopAllApps() } }

        // The warm rebuild: a fresh service over the same state directory,
        // loading the live pid as `persistedPID`. Ticks fast so a real one
        // fires — the previous test pinned the interval at 600 s and so never
        // exercised the loop where this bug lives.
        let pids = PIDStub(paths: [livePID: "\(appsBase)/\(appID)/sleep.sh"])
        let warm = makeService(
            appsBase: appsBase,
            supervisorInterval: .milliseconds(50),
            restartFloor: .zero,
            pids: pids
        )
        #expect(await warm.appInfo(forAppID: appID)?.status == .stopped)

        let supervisor = WendyAgent.makeAppSupervisorTask(
            containerService: warm,
            reconcile: false
        )
        defer { supervisor.cancel() }

        try await waitForSupervisor("the warm service to adopt the live app") {
            await warm.appInfo(forAppID: appID)?.status == .running
        }

        // Let several more ticks run: adoption has to be stable, not a
        // one-shot that a later tick undoes into a relaunch.
        try await Task.sleep(for: .milliseconds(300))

        // Adopted, not relaunched: same pid, and nothing was signalled — that
        // process belongs to the other service.
        #expect(await warm.appInfo(forAppID: appID)?.pid == livePID)
        #expect(pids.sentSignals().isEmpty)
        #expect(await owner.appInfo(forAppID: appID)?.pid == livePID)
    }

    @Test("an adopted native app can be stopped by pid")
    func adoptedNativeAppCanBeStopped() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.StopAdoptedNative"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        let pids = PIDStub(paths: [persistedPID: "\(appsBase)/\(appID)/sleep.sh"])
        let warm = makeService(appsBase: appsBase, restartFloor: .zero, pids: pids)

        await warm.superviseApps()
        #expect(await warm.appInfo(forAppID: appID)?.status == .running)
        #expect(await warm.appInfo(forAppID: appID)?.pid == persistedPID)

        try await stopNativeApp(service: warm, appID: appID)

        #expect(pids.sentSignals().map(\.pid) == [persistedPID])
        #expect(pids.sentSignals().map(\.signal) == [SIGTERM])
        #expect(await warm.appInfo(forAppID: appID)?.status == .stopped)
    }

    @Test("a persisted pid whose executable does not match is neither adopted nor signalled")
    func supervisorDoesNotAdoptAnUnrelatedPID() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.NoAdoptUnrelated"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        // `.no` policy isolates the adoption decision: the restart loop can't
        // relaunch the app, so the status afterwards reflects only whether it
        // was adopted.
        var policy = RestartPolicy()
        policy.mode = .no
        let service = makeService(appsBase: appsBase)
        try await createNativeApp(
            service: service,
            appID: appID,
            cmd: "sleep.sh",
            restartPolicy: policy
        )
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        // The kernel handed the number to something else entirely.
        let pids = PIDStub(paths: [persistedPID: "/usr/bin/some-unrelated-tool"])
        let warm = makeService(appsBase: appsBase, restartFloor: .zero, pids: pids)

        await warm.superviseApps()

        #expect(await warm.appInfo(forAppID: appID)?.status == .stopped)
        #expect(pids.sentSignals().isEmpty)
    }

    @Test("supervisor does not restart an ON_FAILURE app that exited cleanly")
    func supervisorSkipsCleanExitUnderOnFailure() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.OnFailureCleanExit"
        try writeSupervisorCrashScript(appsBase: appsBase, appID: appID, name: "exit.sh", code: 0)

        var policy = RestartPolicy()
        policy.mode = .onFailure
        let service = makeService(appsBase: appsBase, restartFloor: .zero)
        try await createNativeApp(
            service: service,
            appID: appID,
            cmd: "exit.sh",
            restartPolicy: policy
        )
        try await startNativeApp(service: service, appID: appID)

        try await waitForSupervisor("app exits cleanly") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }
        #expect(await service.lastExitCode(forAppID: appID) == 0)

        await service.superviseApps()

        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
        #expect(await service.failureCount(forAppID: appID) == 0)
    }

    @Test("supervisor restarts an ON_FAILURE app until onFailureMaxRetries is reached")
    func supervisorHonorsOnFailureMaxRetries() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.OnFailureRetries"
        try writeSupervisorCrashScript(appsBase: appsBase, appID: appID, name: "crash.sh", code: 9)

        var policy = RestartPolicy()
        policy.mode = .onFailure
        policy.onFailureMaxRetries = 2
        let service = makeService(appsBase: appsBase, restartFloor: .zero)
        try await createNativeApp(
            service: service,
            appID: appID,
            cmd: "crash.sh",
            restartPolicy: policy
        )
        try await startNativeApp(service: service, appID: appID)

        // Two restarts are allowed; the third tick must not fire one.
        for expectedFailureCount in 1...2 {
            try await waitForSupervisor("app crashes (attempt \(expectedFailureCount))") {
                await service.appInfo(forAppID: appID)?.status == .stopped
            }
            #expect(await service.lastExitCode(forAppID: appID) == 9)

            await service.superviseApps()
            #expect(await service.appInfo(forAppID: appID)?.status == .running)
            #expect(await service.failureCount(forAppID: appID) == expectedFailureCount)
        }

        try await waitForSupervisor("app crashes past its retry budget") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }
        await service.superviseApps()

        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
        #expect(await service.failureCount(forAppID: appID) == 2)
    }

    @Test("supervisor does nothing once the agent has begun stopping")
    func supervisorIsInertWhileStopping() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.SupervisorStopping"
        try writeSupervisorCrashScript(appsBase: appsBase, appID: appID, name: "crash.sh", code: 1)

        let service = makeService(appsBase: appsBase, restartFloor: .zero)
        try await createNativeApp(service: service, appID: appID, cmd: "crash.sh")
        try await startNativeApp(service: service, appID: appID)

        try await waitForSupervisor("app crashes on its own") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }

        await service.beginStopping()
        await service.superviseApps()

        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
        #expect(await service.failureCount(forAppID: appID) == 0)
    }

    @Test("startAppUnattended refuses to launch once the agent has begun stopping")
    func startAppUnattendedRefusesToLaunchAfterBeginStopping() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        // Calls `startAppUnattended` directly, bypassing the `isStopping`
        // guards already at the top of `superviseApps`/`reconcileApps`
        // (covered by `supervisorIsInertWhileStopping` and
        // `reconcileIsInertWhileStopping`), to isolate the guard added inside
        // `startAppUnattended` itself.
        let appID = "svc-unattended-stopping"
        let backend = StubLinuxBackend()
        let service = makeService(appsBase: appsBase, backend: backend)
        try await createLinuxApp(service: service, appID: appID)

        await service.beginStopping()
        await service.startAppUnattendedForTesting(id: appID)

        #expect(await backend.pullCount() == 0)
        #expect(await backend.startedApps().isEmpty)
        #expect(await service.appInfo(forAppID: appID)?.status == .stopped)
    }

    @Test("reconcile does nothing once the agent has begun stopping")
    func reconcileIsInertWhileStopping() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.ReconcileStopping"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")

        let restarted = makeService(appsBase: appsBase)
        await restarted.beginStopping()
        await restarted.reconcileApps()

        #expect(await restarted.appInfo(forAppID: appID)?.status == .stopped)
    }

    // MARK: - Reporting

    @Test("listContainers reports CRASH_LOOPING for a down app that will be restarted")
    func listContainersReportsCrashLooping() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let crashingID = "sh.wendy.tests.CrashLooping"
        let stoppedID = "sh.wendy.tests.UserStopped"
        try writeSupervisorCrashScript(
            appsBase: appsBase,
            appID: crashingID,
            name: "crash.sh",
            code: 2
        )
        try writeSupervisorSleepScript(appsBase: appsBase, appID: stoppedID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase, restartFloor: .seconds(10))
        try await createNativeApp(service: service, appID: crashingID, cmd: "crash.sh")
        try await createNativeApp(service: service, appID: stoppedID, cmd: "sleep.sh")

        try await startNativeApp(service: service, appID: crashingID)
        try await waitForSupervisor("crashing app exits") {
            await service.appInfo(forAppID: crashingID)?.status == .stopped
        }
        await service.superviseApps()
        try await waitForSupervisor("crashing app exits again") {
            await service.appInfo(forAppID: crashingID)?.status == .stopped
        }

        try await startNativeApp(service: service, appID: stoppedID)
        try await stopNativeApp(service: service, appID: stoppedID)

        let containers = try await listSupervisorContainers(service: service)
        let crashing = try #require(containers.first { $0.appName == crashingID })
        let userStopped = try #require(containers.first { $0.appName == stoppedID })

        #expect(crashing.runningState == .crashLooping)
        #expect(userStopped.runningState == .stopped)
    }

    // MARK: - Native survivors

    @Test("reconcile never signals a persisted pid whose executable does not match")
    func reconcileLeavesUnrelatedPIDAlone() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.SurvivorMismatch"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        // The kernel handed the pid to something else entirely.
        let pids = PIDStub(paths: [persistedPID: "/usr/bin/some-unrelated-tool"])
        let restarted = makeService(appsBase: appsBase, pids: pids)
        #expect(await restarted.persistedPID(forAppID: appID) == persistedPID)

        await restarted.reconcileApps()

        #expect(pids.sentSignals().isEmpty)
        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)
        await restarted.stopAllApps()
    }

    @Test(
        "reconcile identifies a surviving script by birth time despite its interpreter executable"
    )
    func reconcileTerminatesMatchingSurvivor() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.SurvivorMatch"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        let service = makeService(appsBase: appsBase)
        try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        // The pid is still running this app's binary: it is ours.
        let binaryPath = "/bin/sh"
        let pids = PIDStub(paths: [persistedPID: binaryPath])
        let restarted = makeService(appsBase: appsBase, pids: pids)

        await restarted.reconcileApps()

        #expect(pids.sentSignals().map(\.pid) == [persistedPID])
        #expect(pids.sentSignals().map(\.signal) == [SIGTERM])
        #expect(await restarted.appInfo(forAppID: appID)?.status == .running)
        await restarted.stopAllApps()
    }

    @Test("a reused PID running the same executable is never signalled or adopted")
    func reusedPIDWithDifferentBirthIsNotOurs() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }
        let appID = "sh.wendy.tests.ReusedPID"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")
        let original = makeService(appsBase: appsBase)
        try await createNativeApp(service: original, appID: appID, cmd: "sleep.sh")
        let pid = try await simulateDisorderlyExit(service: original, appID: appID)
        let pids = PIDStub(paths: [pid: "\(appsBase)/\(appID)/sleep.sh"])
        pids.setBirth(pid, 1)
        let restarted = makeService(appsBase: appsBase, pids: pids)
        await restarted.reconcileApps()
        #expect(pids.sentSignals().isEmpty)
        #expect(await restarted.appInfo(forAppID: appID)?.pid != pid)
        await restarted.stopAllApps()
    }

    @Test("reconcile terminates the survivor of an app it will not restart")
    func reconcileTerminatesSurvivorOfNoRestartApp() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "sh.wendy.tests.SurvivorNoRestart"
        try writeSupervisorSleepScript(appsBase: appsBase, appID: appID, name: "sleep.sh")

        var policy = RestartPolicy()
        policy.mode = .no
        let service = makeService(appsBase: appsBase)
        try await createNativeApp(
            service: service,
            appID: appID,
            cmd: "sleep.sh",
            restartPolicy: policy
        )
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        let binaryPath = "\(appsBase)/\(appID)/sleep.sh"
        let pids = PIDStub(paths: [persistedPID: binaryPath])
        let restarted = makeService(appsBase: appsBase, pids: pids)

        await restarted.reconcileApps()

        // The policy keeps it from being restarted, but the process that
        // outlived the crash must not be left running behind the agent's back.
        #expect(pids.sentSignals().map(\.pid) == [persistedPID])
        #expect(await restarted.appInfo(forAppID: appID)?.status == .stopped)
    }

    @Test("reconcile ignores a persisted pid for a container app")
    func reconcileIgnoresPersistedPIDForContainers() async throws {
        let appsBase = try makeSupervisorTempDir()
        defer { cleanupSupervisorPath(appsBase) }

        let appID = "svc-no-signal"
        let backend = StubLinuxBackend(listing: [])
        let service = makeService(appsBase: appsBase, backend: backend)
        try await createLinuxApp(service: service, appID: appID)
        let persistedPID = try await simulateDisorderlyExit(service: service, appID: appID)

        // Even a perfectly matching pid must not be signalled for a container
        // app: the backend owns that lifecycle.
        let pids = PIDStub(paths: [persistedPID: "/bin/sleep"])
        let restarted = makeService(appsBase: appsBase, backend: backend, pids: pids)
        #expect(await restarted.persistedPID(forAppID: appID) == persistedPID)
        await restarted.reconcileApps()

        #expect(pids.sentSignals().isEmpty)

        await restarted.stopAllApps()
        await backend.terminateAll()
    }
}

// MARK: - Helpers

private func makeService(
    appsBase: String,
    backend: (any LinuxContainerBackend)? = nil,
    supervisorInterval: Duration = .seconds(15),
    restartFloor: Duration = .seconds(10),
    pids: PIDStub? = nil
) -> ContainerService {
    let lookup: PIDExecutablePathLookup
    let send: PIDSignalSender
    let birthLookup: PIDBirthTimeLookup
    if let pids {
        if let data = try? Data(
            contentsOf: URL(fileURLWithPath: appsBase).appendingPathComponent("info.json")
        ),
            let apps = try? JSONDecoder().decode([WendyApp].self, from: data)
        {
            pids.restoreBirths(apps)
        }
        birthLookup = pids.birthLookup
        lookup = pids.lookup
        send = pids.send
    } else {
        birthLookup = { NativeProcessConfiguration.birthTime(forPID: $0) }
        lookup = { ContainerService.executablePath(forPID: $0) }
        send = { pid, signal in _ = Darwin.kill(pid, signal) }
    }

    return ContainerService(
        broadcaster: TelemetryBroadcaster(),
        executablePath: "/usr/bin/false",
        appsBase: URL(fileURLWithPath: appsBase),
        linuxBackend: backend,
        supervisorInterval: supervisorInterval,
        restartFloor: restartFloor,
        pidExecutablePath: lookup,
        pidBirthTime: birthLookup,
        sendSignal: send
    )
}

/// Registers a native app in `appsBase`, leaves behind the `info.json` a
/// disorderly agent exit would (status running + pid), and returns a `PIDStub`
/// that reports that pid as still running the app's binary.
private func stageSurvivor(appsBase: String, appID: String) async throws -> PIDStub {
    let service = makeService(appsBase: appsBase)
    try await createNativeApp(service: service, appID: appID, cmd: "sleep.sh")
    let pid = try await simulateDisorderlyExit(service: service, appID: appID)
    return PIDStub(paths: [pid: "\(appsBase)/\(appID)/sleep.sh"])
}

private func createNativeApp(
    service: ContainerService,
    appID: String,
    cmd: String,
    restartPolicy: RestartPolicy? = nil
) async throws {
    var request = Wendy_Agent_Services_V1_CreateContainerRequest()
    request.appName = appID
    request.imageName = ""
    request.cmd = cmd
    if let restartPolicy {
        request.restartPolicy = restartPolicy
    }

    _ = try await service.createContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeSupervisorServerContext(method: "CreateContainer")
    )
}

private func createLinuxApp(
    service: ContainerService,
    appID: String,
    restartPolicy: RestartPolicy? = nil
) async throws {
    let config = WendyAppConfig(
        appId: appID,
        platform: "linux/arm64",
        entitlements: nil,
        brewfile: nil
    )
    var request = Wendy_Agent_Services_V1_CreateContainerRequest()
    request.appName = appID
    request.imageName = "localhost:5555/\(appID):latest"
    request.appConfig = try JSONEncoder().encode(config)
    if let restartPolicy {
        request.restartPolicy = restartPolicy
    }

    _ = try await service.createContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeSupervisorServerContext(method: "CreateContainer")
    )
}

private func startNativeApp(service: ContainerService, appID: String) async throws {
    var request = Wendy_Agent_Services_V1_StartContainerRequest()
    request.appName = appID
    _ = try await service.startContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeSupervisorServerContext(method: "StartContainer")
    )
}

/// Starts `appID`, snapshots the `info.json` the agent wrote while it was
/// running (status `running` + pid), then stops it cleanly and restores that
/// snapshot. The result is the on-disk state a *disorderly* agent exit leaves
/// behind — the only case where a persisted pid survives — without having to
/// actually crash the test process. Returns the pid recorded in the snapshot.
private func simulateDisorderlyExit(
    service: ContainerService,
    appID: String
) async throws -> Int32 {
    try await startNativeApp(service: service, appID: appID)
    let infoFileURL = await service.infoFileURLForTesting()
    let runningSnapshot = try Data(contentsOf: infoFileURL)
    let pid = try #require(await service.appInfo(forAppID: appID)?.pid)

    await service.stopAllApps()
    try runningSnapshot.write(to: infoFileURL, options: .atomic)
    return pid
}

private func stopNativeApp(service: ContainerService, appID: String) async throws {
    var request = Wendy_Agent_Services_V1_StopContainerRequest()
    request.appName = appID
    _ = try await service.stopContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeSupervisorServerContext(method: "StopContainer")
    )
}

private func listSupervisorContainers(service: ContainerService) async throws -> [AppContainer] {
    let response = try await service.listContainers(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V1_ListContainersRequest()
        ),
        context: makeSupervisorServerContext(method: "ListContainers")
    )

    let contents = try response.accepted.get()
    let writer = SupervisorCollectingWriter<Wendy_Agent_Services_V1_ListContainersResponse>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))
    return writer.snapshot().compactMap(\.container)
}

private final class SupervisorCollectingWriter<Element: Sendable>: RPCWriterProtocol,
    @unchecked Sendable
{
    private let queue = DispatchQueue(label: "wendy.tests.supervisor-collecting-writer")
    private var elements: [Element] = []

    func write(_ element: Element) async throws {
        queue.sync { elements.append(element) }
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        queue.sync { self.elements.append(contentsOf: elements) }
    }

    func snapshot() -> [Element] {
        queue.sync { elements }
    }
}

private func makeSupervisorServerContext(method: String) -> ServerContext {
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

private struct SupervisorTestError: Error, CustomStringConvertible {
    let description: String
}

private func makeSupervisorTempDir() throws -> String {
    let path =
        FileManager.default.temporaryDirectory
        .appendingPathComponent("wendy-supervisor-\(UUID().uuidString)").path
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
    return path
}

private func cleanupSupervisorPath(_ path: String) {
    try? FileManager.default.removeItem(atPath: path)
}

/// Plants an `info.json` in the shape an agent from before restart parity
/// wrote: no `restartPolicy`, no `stoppedByUser`.
private func writeLegacyInfoJSON(appsBase: String, appID: String, binaryName: String) throws {
    let json = legacyInfoJSON(appID: appID, binaryName: binaryName)
        .replacingOccurrences(of: "/tmp/\(appID)", with: "\(appsBase)/\(appID)")
    try json.write(
        to: URL(fileURLWithPath: appsBase).appendingPathComponent("info.json"),
        atomically: true,
        encoding: .utf8
    )
}

private func writeSupervisorSleepScript(appsBase: String, appID: String, name: String) throws {
    try writeSupervisorScript(
        appsBase: appsBase,
        appID: appID,
        name: name,
        body: "while true; do\n  sleep 1\ndone\n"
    )
}

private func writeSupervisorCrashScript(
    appsBase: String,
    appID: String,
    name: String,
    code: Int
) throws {
    try writeSupervisorScript(
        appsBase: appsBase,
        appID: appID,
        name: name,
        body: "sleep 0.2\nexit \(code)\n"
    )
}

private func writeSupervisorScript(
    appsBase: String,
    appID: String,
    name: String,
    body: String
) throws {
    let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
    try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
    let scriptURL = appDirectory.appendingPathComponent(name)
    try "#!/bin/sh\n\(body)".write(to: scriptURL, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes(
        [.posixPermissions: 0o755],
        ofItemAtPath: scriptURL.path
    )
}

private func waitForSupervisor(
    _ description: String,
    timeout: Duration = .seconds(5),
    pollInterval: Duration = .milliseconds(20),
    condition: @escaping @Sendable () async -> Bool
) async throws {
    let clock = ContinuousClock()
    let deadline = clock.now + timeout

    while clock.now < deadline {
        if await condition() { return }
        try await Task.sleep(for: pollInterval)
    }

    throw SupervisorTestError(description: "Timed out waiting for \(description)")
}
