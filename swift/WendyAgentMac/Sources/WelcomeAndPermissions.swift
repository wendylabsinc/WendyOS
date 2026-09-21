import AVFoundation
import AppKit
import CoreBluetooth
import CoreLocation
import OSLog
import Observation
import ServiceManagement

@Observable
@MainActor
final class WelcomeAndPermissions: NSObject, CBCentralManagerDelegate, CLLocationManagerDelegate {
    enum Permission: CaseIterable, Hashable {
        case bluetooth
        case camera
        case microphone
        case location

        var title: String {
            switch self {
            case .bluetooth:
                return "Bluetooth"
            case .camera:
                return "Camera"
            case .microphone:
                return "Microphone"
            case .location:
                return "Location"
            }
        }

        var subtitle: String {
            switch self {
            case .bluetooth:
                return "Device discovery and transfers"
            case .camera:
                return "For Wendy apps that use video"
            case .microphone:
                return "For Wendy apps that use audio"
            case .location:
                return "Wi-Fi network scanning"
            }
        }

        var systemImage: String {
            switch self {
            case .bluetooth:
                return "dot.radiowaves.left.and.right"
            case .camera:
                return "camera"
            case .microphone:
                return "mic"
            case .location:
                return "location"
            }
        }
    }

    enum PermissionStatus: Equatable {
        case pending
        case allowed
        case denied
        case restricted
    }

    private static let launchAtLoginEnabledKey = "launchAtLoginEnabled"
    private static let locationRequestTimeout = Duration.seconds(60)

    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier ?? "sh.wendy.WendyAgentMac",
        category: "WelcomeAndPermissions"
    )

    var launchAtLoginEnabled: Bool
    var bluetoothStatus: PermissionStatus = .pending
    var cameraStatus: PermissionStatus = .pending
    var microphoneStatus: PermissionStatus = .pending
    var locationStatus: PermissionStatus = .pending
    var requestingPermission: Permission?

    private var bluetoothManager: CBCentralManager?
    private var bluetoothContinuation: CheckedContinuation<PermissionStatus, Never>?
    private var locationManager: CLLocationManager?
    private var locationContinuation: CheckedContinuation<PermissionStatus, Never>?
    private var locationRequestTimeoutTask: Task<Void, Never>?

    override init() {
        self.launchAtLoginEnabled = Self.currentLaunchAtLoginEnabled()

        super.init()
        self.refreshPermissionStatuses()
    }

    var shouldShowWelcomeAndPermissions: Bool {
        self.currentBluetoothStatus() == .pending
            || self.currentCameraStatus() == .pending
            || self.currentMicrophoneStatus() == .pending
            || self.currentLocationStatus() == .pending
    }

    var canFinish: Bool {
        self.bluetoothStatus == .allowed
            && self.cameraStatus == .allowed
            && self.microphoneStatus == .allowed
            && self.locationStatus == .allowed
    }

    var isWorking: Bool {
        self.requestingPermission != nil
    }

    func configureLaunchAtLoginOnStartup() {
        self.applyLaunchAtLoginPreference(enabled: Self.currentLaunchAtLoginPreference())
    }

    func prepareForPresentation() {
        self.requestingPermission = nil
        self.refresh()
    }

    func refresh() {
        self.refreshLaunchAtLoginStatus()
        self.refreshPermissionStatuses()
    }

    func setLaunchAtLoginEnabled(_ enabled: Bool) {
        self.launchAtLoginEnabled = enabled
        UserDefaults.standard.set(enabled, forKey: Self.launchAtLoginEnabledKey)
        self.applyLaunchAtLoginPreference(enabled: enabled)
    }

    func requestPermission(_ permission: Permission) async {
        guard self.requestingPermission == nil else { return }

        self.requestingPermission = permission
        defer {
            self.requestingPermission = nil
            self.refreshPermissionStatuses()
        }

        switch permission {
        case .bluetooth:
            self.bluetoothStatus = await self.requestBluetoothAccess()
        case .camera:
            self.cameraStatus = await self.requestCameraAccess()
        case .microphone:
            self.microphoneStatus = await self.requestMicrophoneAccess()
        case .location:
            self.locationStatus = await self.requestLocationAccess()
        }
    }

    func status(for permission: Permission) -> PermissionStatus {
        switch permission {
        case .bluetooth:
            return self.bluetoothStatus
        case .camera:
            return self.cameraStatus
        case .microphone:
            return self.microphoneStatus
        case .location:
            return self.locationStatus
        }
    }

    func openSystemSettings(for permission: Permission) {
        guard let settingsURL = self.systemSettingsURL(for: permission) else {
            return
        }

        guard NSWorkspace.shared.open(settingsURL) else {
            self.logger.error(
                "Failed to open System Settings for permission: \(String(describing: permission), privacy: .public)"
            )
            return
        }
    }

    func centralManagerDidUpdateState(_ central: CBCentralManager) {
        guard let bluetoothContinuation = self.bluetoothContinuation else { return }

        self.bluetoothContinuation = nil
        self.bluetoothManager = nil
        bluetoothContinuation.resume(returning: self.currentBluetoothStatus())
    }

    nonisolated func locationManagerDidChangeAuthorization(_ manager: CLLocationManager) {
        // Read the Sendable status here, in the nonisolated context: the
        // non-Sendable `manager` must not be captured into the MainActor hop.
        let authorizationStatus = manager.authorizationStatus
        Task { @MainActor [weak self] in
            guard let self else { return }

            self.locationStatus = self.locationStatus(for: authorizationStatus)
            self.finishLocationRequestIfResolved()
        }
    }

    private func refreshPermissionStatuses() {
        self.bluetoothStatus = self.currentBluetoothStatus()
        self.cameraStatus = self.currentCameraStatus()
        self.microphoneStatus = self.currentMicrophoneStatus()
        self.locationStatus = self.currentLocationStatus()
        self.finishLocationRequestIfResolved()
    }

    private func finishLocationRequestIfResolved(allowPending: Bool = false) {
        guard allowPending || self.locationStatus != .pending,
            let locationContinuation = self.locationContinuation
        else {
            return
        }

        self.locationContinuation = nil
        self.locationManager = nil
        self.locationRequestTimeoutTask?.cancel()
        self.locationRequestTimeoutTask = nil
        locationContinuation.resume(returning: self.locationStatus)
    }

    private func systemSettingsURL(for permission: Permission) -> URL? {
        let privacyPane: String
        switch permission {
        case .bluetooth:
            privacyPane = "Privacy_Bluetooth"
        case .camera:
            privacyPane = "Privacy_Camera"
        case .microphone:
            privacyPane = "Privacy_Microphone"
        case .location:
            privacyPane = "Privacy_LocationServices"
        }

        return URL(string: "x-apple.systempreferences:com.apple.preference.security?\(privacyPane)")
    }

    private func refreshLaunchAtLoginStatus() {
        self.launchAtLoginEnabled = Self.currentLaunchAtLoginEnabled()
    }

    private func applyLaunchAtLoginPreference(enabled: Bool) {
        let loginItemService = SMAppService.mainApp

        defer {
            self.refreshLaunchAtLoginStatus()
        }

        do {
            if enabled {
                switch loginItemService.status {
                case .enabled:
                    self.logger.info("Wendy Agent is already configured to launch at login")
                case .notRegistered, .requiresApproval, .notFound:
                    try loginItemService.register()
                    self.logLaunchAtLoginRegistrationStatus(loginItemService.status)
                @unknown default:
                    try loginItemService.register()
                    self.logLaunchAtLoginRegistrationStatus(loginItemService.status)
                }
            } else {
                switch loginItemService.status {
                case .enabled, .requiresApproval:
                    try loginItemService.unregister()
                    self.logger.info("Disabled Wendy Agent launch at login")
                case .notRegistered, .notFound:
                    break
                @unknown default:
                    try loginItemService.unregister()
                    self.logger.info("Disabled Wendy Agent launch at login")
                }
            }
        } catch {
            self.logger.error(
                "Failed to configure Wendy Agent launch at login: \(String(describing: error), privacy: .public)"
            )
        }
    }

    private func logLaunchAtLoginRegistrationStatus(_ status: SMAppService.Status) {
        switch status {
        case .enabled:
            self.logger.info("Configured Wendy Agent to launch at login")
        case .requiresApproval:
            self.logger.notice(
                "Wendy Agent launch at login requires user approval in System Settings"
            )
        case .notRegistered, .notFound:
            self.logger.warning(
                "Wendy Agent launch at login registration did not complete; status: \(String(describing: status), privacy: .public)"
            )
        @unknown default:
            self.logger.warning(
                "Wendy Agent launch at login registration returned an unknown status"
            )
        }
    }

    private static func currentLaunchAtLoginEnabled() -> Bool {
        SMAppService.mainApp.status == .enabled
    }

    private static func currentLaunchAtLoginPreference() -> Bool {
        let defaults = UserDefaults.standard
        if defaults.object(forKey: Self.launchAtLoginEnabledKey) == nil {
            return true
        }

        return defaults.bool(forKey: Self.launchAtLoginEnabledKey)
    }

    private func currentBluetoothStatus() -> PermissionStatus {
        switch CBCentralManager.authorization {
        case .allowedAlways:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            return .pending
        @unknown default:
            return .pending
        }
    }

    private func currentLocationStatus() -> PermissionStatus {
        let manager = self.locationManager ?? CLLocationManager()
        return self.locationStatus(for: manager.authorizationStatus)
    }

    private func locationStatus(for authorizationStatus: CLAuthorizationStatus) -> PermissionStatus
    {
        switch authorizationStatus {
        case .authorizedAlways:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            return .pending
        @unknown default:
            return .pending
        }
    }

    private func currentCameraStatus() -> PermissionStatus {
        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .authorized:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            return .pending
        @unknown default:
            return .pending
        }
    }

    private func currentMicrophoneStatus() -> PermissionStatus {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            return .pending
        @unknown default:
            return .pending
        }
    }

    private func requestBluetoothAccess() async -> PermissionStatus {
        switch CBCentralManager.authorization {
        case .allowedAlways:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            return await withCheckedContinuation { continuation in
                self.bluetoothContinuation = continuation
                self.bluetoothManager = CBCentralManager(delegate: self, queue: nil)
            }
        @unknown default:
            return .pending
        }
    }

    private func requestLocationAccess() async -> PermissionStatus {
        let current = self.currentLocationStatus()
        guard current == .pending else { return current }

        return await withCheckedContinuation { continuation in
            self.locationContinuation = continuation
            let manager = CLLocationManager()
            manager.delegate = self
            self.locationManager = manager
            manager.requestWhenInUseAuthorization()

            self.locationRequestTimeoutTask = Task { [weak self] in
                do {
                    try await Task.sleep(for: Self.locationRequestTimeout)
                } catch {
                    return
                }

                guard let self else { return }
                self.locationStatus = self.currentLocationStatus()
                self.finishLocationRequestIfResolved(allowPending: true)
            }
        }
    }

    private func requestCameraAccess() async -> PermissionStatus {
        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .authorized:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            let granted = await AVCaptureDevice.requestAccess(for: .video)
            return granted ? .allowed : .denied
        @unknown default:
            return .pending
        }
    }

    private func requestMicrophoneAccess() async -> PermissionStatus {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized:
            return .allowed
        case .denied:
            return .denied
        case .restricted:
            return .restricted
        case .notDetermined:
            let granted = await AVCaptureDevice.requestAccess(for: .audio)
            return granted ? .allowed : .denied
        @unknown default:
            return .pending
        }
    }
}
