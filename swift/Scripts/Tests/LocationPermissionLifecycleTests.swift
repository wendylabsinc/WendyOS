import CoreLocation
import Darwin
import Foundation
import Synchronization

// Only native facts and the request submission are controlled. The production
// observable model, request state, delegate handling and 60-second timer run.
@MainActor
enum LocationFixtureFacts {
    static var description: String? = "Reserved fixture location explanation"
    static var enabled = true
}

nonisolated final class LifecycleLocationManager: CLLocationManager {
    struct State { var status = CLAuthorizationStatus.notDetermined; var calls = 0 }
    private let state = Mutex(State())
    var nativeStatus: CLAuthorizationStatus {
        get { state.withLock { $0.status } }
        set { state.withLock { $0.status = newValue } }
    }
    var requests: Int { state.withLock { $0.calls } }
    override var authorizationStatus: CLAuthorizationStatus { nativeStatus }
    override func requestWhenInUseAuthorization() { state.withLock { $0.calls += 1 } }
}
@MainActor
final class LifecycleLogin: LaunchAtLoginSetting {
    var isEnabled = false
    func setEnabled(_ enabled: Bool) { isEnabled = enabled }
}

@main
struct LocationPermissionLifecycleTests {
    @MainActor static func model() -> (WelcomeAndPermissions, LifecycleLocationManager) {
        LocationFixtureFacts.description = "Reserved fixture location explanation"
        LocationFixtureFacts.enabled = true
        let source = LifecycleLocationManager()
        return (WelcomeAndPermissions(locationManager: source, launchAtLogin: LifecycleLogin()), source)
    }
    @MainActor static func until(_ message: String, _ condition: () -> Bool) async {
        let end = ContinuousClock.now + .seconds(2)
        while !condition() && ContinuousClock.now < end { try? await Task.sleep(for: .milliseconds(5)) }
        precondition(condition(), message)
    }
    @MainActor static func pendingDecision() async {
        let (model, source) = model()
        var returned = false
        let task = Task { @MainActor in await model.requestPermission(.location); returned = true }
        await until("native request not submitted") { source.requests == 1 }
        // Submission, refresh and an unanswered callback must all retain the wait.
        model.refresh()
        model.locationManagerDidChangeAuthorization(source)
        try? await Task.sleep(for: .milliseconds(150))
        precondition(model.isWorking && !returned, "submission falsely completed native Location decision")
        await model.requestPermission(.location)
        precondition(source.requests == 1, "duplicate request escaped pending guard")
        source.nativeStatus = .denied
        model.locationManagerDidChangeAuthorization(source)
        await until("denial did not finish request") { returned }
        await task.value
        precondition(model.locationStatus == .denied && !model.isWorking && model.locationRequestMessage == nil)
        // A Settings change is read from native state and observed on refresh.
        source.nativeStatus = .authorizedAlways
        model.refresh()
        precondition(model.locationStatus == .allowed && source.requests == 1)
        print("LOCATION_MODEL_PENDING_DENIAL_RECOVERY_VERIFIED")
    }
    @MainActor static func preconditions() async {
        let (model, source) = model()
        LocationFixtureFacts.description = nil
        await model.requestPermission(.location)
        precondition(source.requests == 0 && !model.isWorking && model.locationRequestMessage == "Wendy cannot request Location access in this build.")
        LocationFixtureFacts.description = "Present"
        LocationFixtureFacts.enabled = false
        await model.requestPermission(.location)
        precondition(source.requests == 0 && !model.isWorking && model.locationRequestMessage == "Location Services are disabled in System Settings.")
        source.nativeStatus = .restricted
        await model.requestPermission(.location)
        precondition(source.requests == 0 && !model.isWorking && model.locationStatus == .restricted)
        print("LOCATION_MODEL_NO_NATIVE_REQUEST_GUARDS_VERIFIED")
    }
    @MainActor static func unanswered() async {
        let (model, source) = model()
        let start = ContinuousClock.now
        var returned = false
        let task = Task { @MainActor in await model.requestPermission(.location); returned = true }
        await until("timeout case never submitted") { source.requests == 1 }
        print("LOCATION_MODEL_UNANSWERED_STARTED actual_timeout=60s")
        fflush(nil)
        await task.value
        let elapsed = start.duration(to: .now)
        precondition(elapsed >= .seconds(59) && elapsed < .seconds(75), "actual Location timeout changed")
        precondition(returned && !model.isWorking && model.locationStatus == .pending)
        precondition(model.locationRequestMessage == "macOS has not returned a Location decision. Try again or check System Settings.", "unanswered native request lacked explanation")
        // A fresh request after timeout must work and clear the fallback message.
        var retried = false
        let retry = Task { @MainActor in await model.requestPermission(.location); retried = true }
        await until("retry was not submitted") { source.requests == 2 }
        precondition(model.isWorking && model.locationRequestMessage == nil)
        source.nativeStatus = .authorizedAlways
        model.locationManagerDidChangeAuthorization(source)
        model.locationManagerDidChangeAuthorization(source)
        await until("approval did not finish retry") { retried }
        await retry.value
        precondition(model.locationStatus == .allowed && !model.isWorking && model.locationRequestMessage == nil)
        print("LOCATION_MODEL_TIMEOUT_RETRY_VERIFIED elapsed=\(elapsed)")
    }
    @MainActor static func main() async {
        switch CommandLine.arguments.last {
        case "pending": await pendingDecision()
        case "timeout": await unanswered()
        default: await pendingDecision(); await preconditions(); await unanswered()
        }
        print("LOCATION_MODEL_LIFECYCLE_PASSED")
    }
}
