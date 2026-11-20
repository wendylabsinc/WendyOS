import BackgroundTasks
import SwiftUI

@MainActor
protocol OSUpdateBackgroundTask: AnyObject {
    func onExpiration(_ action: @escaping @MainActor @Sendable () -> Void)
    func report(_ install: OSUpdateSnapshot.Install)
    func finish(success: Bool)
}

@MainActor
private final class SystemOSUpdateTask: OSUpdateBackgroundTask {
    let task: BGContinuedProcessingTask
    init(_ task: BGContinuedProcessingTask) { self.task = task }
    func onExpiration(_ action: @escaping @MainActor @Sendable () -> Void) {
        task.expirationHandler = { DispatchQueue.main.async { action() } }
    }
    func report(_ install: OSUpdateSnapshot.Install) {
        task.progress.totalUnitCount = install.fraction == nil ? -1 : 100
        task.progress.completedUnitCount = Int64(max(0, min(100, install.percent)))
        task.updateTitle(task.title, subtitle: install.rebootRequired ? "Installed. Restart device to finish." : install.phase.capitalized)
    }
    func finish(success: Bool) {
        task.expirationHandler = nil
        task.setTaskCompleted(success: success)
    }
}

/// One user-requested installation at a time. The scheduler grants runtime before
/// the RPC starts; the app scene owns its worker independently of navigation.
@MainActor
final class MobileOSUpdateTask: ObservableObject {
    static let shared = MobileOSUpdateTask()
    static let identifier = "dev.wendy.WendyCompanion.os-update"
    typealias Launch = @MainActor @Sendable (any OSUpdateBackgroundTask) -> Void
    typealias Submit = @MainActor (String, @escaping Launch) throws -> Void
    private let actions = AsyncActions()
    private let update: (UInt64, String, Bool) async throws -> OSUpdateSnapshot
    private let cancelUpdate: (UInt64) -> Void
    private let submit: Submit
    private var workerRunning = false
    private var request: UUID?
    private var task: (any OSUpdateBackgroundTask)?
    @Published private(set) var active = false
    @Published private(set) var session: UInt64 = 0
    @Published private(set) var scope = ""
    @Published private(set) var snapshot: OSUpdateSnapshot?
    @Published private(set) var error: String?

    init(submit: @escaping Submit = { name, launch in try OSUpdateScheduler.shared.submit(name: name, launch: launch) },
         update: @escaping (UInt64, String, Bool) async throws -> OSUpdateSnapshot = { try await GoClient.shared.osUpdate(session: $0, artifact: $1, start: $2) },
         cancelUpdate: @escaping (UInt64) -> Void = { WDOSUpdateCancel($0) }) {
        self.submit = submit; self.update = update; self.cancelUpdate = cancelUpdate
    }

    func run() async {
        await actions.run()
        if let request { interrupt(id: request, message: "Update monitoring stopped. Reconnect and check the device's update status.") }
    }
    func keepsConnection(session: UInt64, scope: String) -> Bool {
        active && self.session == session && self.scope == scope
    }
    func start(session: UInt64, scope: String, name: String, artifact: String) throws {
        guard !active && !workerRunning else { throw WendyError(message: "An OS update is already running.") }
        let id = UUID()
        WDOSUpdatePrepare(session)
        request = id; self.session = session; self.scope = scope
        snapshot = nil; error = nil; active = true
        do {
            try submit(name) { [weak self] task in
                guard let self, self.request == id else { task.finish(success: false); return }
                self.task = task
                task.onExpiration { [weak self] in
                    self?.interrupt(id: id, message: "Background update stopped by iOS or cancelled. Reconnect and check the device's update status before retrying.")
                }
                self.actions.submit { [weak self] in await self?.install(id: id, session: session, artifact: artifact) }
            }
        } catch {
            request = nil; active = false
            throw WendyError(message: "iOS could not start the background update. No installation was started. Try again. \(error.localizedDescription)")
        }
    }
    func selectionChanged(session: UInt64, scope: String) {
        if active && !keepsConnection(session: session, scope: scope), let request {
            interrupt(id: request, message: "Update connection changed. Reconnect to the original device and check its update status.")
        }
    }
    private func interrupt(id: UUID, message: String) {
        guard request == id else { return }
        cancelUpdate(session)
        error = message
        finish(id: id, success: false)
    }
    private func finish(id: UUID, success: Bool) {
        guard request == id else { return }
        task?.finish(success: success); task = nil; request = nil; active = false
    }
    private func install(id: UUID, session: UInt64, artifact: String) async {
        guard request == id, !Task.isCancelled else { return }
        workerRunning = true
        defer { workerRunning = false }
        defer { if request == id { interrupt(id: id, message: "Update monitoring stopped. Reconnect and check the device's update status.") } }
        do {
            var value = try await update(session, artifact, true)
            while true {
                guard request == id, !Task.isCancelled else { cancelUpdate(session); return }
                snapshot = value; task?.report(value.install)
                if !value.install.active {
                    error = value.install.error.isEmpty ? nil : value.install.error
                    finish(id: id, success: value.install.error.isEmpty && value.install.phase == "Installed")
                    return
                }
                try await Task.sleep(for: .seconds(3))
                guard request == id else { return }
                value = try await update(session, "", false)
            }
        } catch {
            guard request == id else { return }
            interrupt(id: id, message: "\(error.localizedDescription) Reconnect and check the device's update status before retrying.")
        }
    }
}

@MainActor
final class OSUpdateScheduler {
    static let shared = OSUpdateScheduler()
    private var launch: MobileOSUpdateTask.Launch?
    private var registered = false
    func register() {
        guard !registered else { return }
        registered = BGTaskScheduler.shared.register(forTaskWithIdentifier: MobileOSUpdateTask.identifier, using: .main) { task in
            MainActor.assumeIsolated {
                guard let task = task as? BGContinuedProcessingTask, let launch = self.launch else {
                    task.setTaskCompleted(success: false); return
                }
                self.launch = nil
                launch(SystemOSUpdateTask(task))
            }
        }
    }
    func submit(name: String, launch: @escaping MobileOSUpdateTask.Launch) throws {
        guard registered else { throw WendyError(message: "Background update registration is unavailable.") }
        self.launch = launch
        let request = BGContinuedProcessingTaskRequest(identifier: MobileOSUpdateTask.identifier, title: "Updating \(name)", subtitle: "Preparing OS update")
        request.strategy = .fail
        do { try BGTaskScheduler.shared.submit(request) }
        catch { self.launch = nil; throw error }
    }
}
