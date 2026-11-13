import CoreLocation
import Observation
import Synchronization

// Substitute the native source itself, not Wendy's projected permission status.
nonisolated final class TestLocationManager: CLLocationManager {
    private let status = Mutex<CLAuthorizationStatus>(.notDetermined)

    var nativeStatus: CLAuthorizationStatus {
        get { self.status.withLock { $0 } }
        set { self.status.withLock { $0 = newValue } }
    }

    override var authorizationStatus: CLAuthorizationStatus {
        self.nativeStatus
    }
}

final class TestLaunchAtLogin: LaunchAtLoginSetting {
    var isEnabled = false
    func setEnabled(_ enabled: Bool) { self.isEnabled = enabled }
}

@main
struct LocationPermissionSourceTests {
    @MainActor
    static func main() async {
        let native = TestLocationManager()
        let model = WelcomeAndPermissions(
            locationManager: native,
            launchAtLogin: TestLaunchAtLogin()
        )
        precondition(model.locationStatus == .pending)

        // Reads must reflect the source even before a delegate notification.
        native.nativeStatus = .authorizedAlways
        precondition(model.locationStatus == .allowed)
        native.nativeStatus = .denied
        precondition(model.status(for: .location) == .denied)
        native.nativeStatus = .restricted
        precondition(model.locationStatus == .restricted)

        let notified = Mutex(false)
        withObservationTracking {
            _ = model.locationStatus
        } onChange: {
            notified.withLock { $0 = true }
        }
        native.nativeStatus = .notDetermined
        model.refresh()
        precondition(notified.withLock { $0 })
        precondition(model.locationStatus == .pending)

        let delegateNotified = Mutex(false)
        withObservationTracking {
            _ = model.locationStatus
        } onChange: {
            delegateNotified.withLock { $0 = true }
        }
        native.nativeStatus = .authorizedAlways
        model.locationManagerDidChangeAuthorization(native)
        // Simulate another native change before the MainActor callback runs.
        native.nativeStatus = .denied
        for _ in 0..<1000 {
            if delegateNotified.withLock({ $0 }) { break }
            await Task.yield()
        }
        precondition(delegateNotified.withLock { $0 })
        precondition(model.locationStatus == .denied)
        print("Location permission source: live reads and Observation notifications passed")
    }
}
