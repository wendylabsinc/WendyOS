import Foundation

#if os(macOS)
import AVFoundation
@preconcurrency import CoreBluetooth

struct MacOSPermissionAuthorizer: PermissionAuthorizing {
    func status(for permission: PermissionKind) async -> PermissionStatus {
        switch permission {
        case .camera:
            return mapCaptureStatus(AVCaptureDevice.authorizationStatus(for: .video))
        case .microphone:
            return mapCaptureStatus(AVCaptureDevice.authorizationStatus(for: .audio))
        case .bluetooth:
            return await MainActor.run {
                mapBluetoothAuthorization(CBCentralManager.authorization)
            }
        }
    }

    func requestAccess(for permission: PermissionKind) async -> PermissionStatus {
        switch permission {
        case .camera:
            return await requestCaptureAccess(for: .video)
        case .microphone:
            return await requestCaptureAccess(for: .audio)
        case .bluetooth:
            return await requestBluetoothAccess()
        }
    }

    private func requestCaptureAccess(for mediaType: AVMediaType) async -> PermissionStatus {
        await withCheckedContinuation { continuation in
            AVCaptureDevice.requestAccess(for: mediaType) { granted in
                continuation.resume(returning: granted ? .granted : .missing)
            }
        }
    }

    private func requestBluetoothAccess() async -> PermissionStatus {
        let requester = await MainActor.run { BluetoothPermissionRequester() }
        return await requester.requestAccess()
    }

    private func mapCaptureStatus(_ status: AVAuthorizationStatus) -> PermissionStatus {
        switch status {
        case .authorized:
            return .granted
        case .notDetermined, .denied:
            return .missing
        case .restricted:
            return .unknown
        @unknown default:
            return .unknown
        }
    }

    @MainActor
    private func mapBluetoothAuthorization(_ authorization: CBManagerAuthorization) -> PermissionStatus {
        switch authorization {
        case .allowedAlways:
            return .granted
        case .notDetermined, .denied:
            return .missing
        case .restricted:
            return .unknown
        @unknown default:
            return .unknown
        }
    }
}

@MainActor
private final class BluetoothPermissionRequester: NSObject, CBCentralManagerDelegate {
    private var continuation: CheckedContinuation<PermissionStatus, Never>?
    private var manager: CBCentralManager?
    private var timeoutTask: Task<Void, Never>?

    func requestAccess() async -> PermissionStatus {
        await withCheckedContinuation { continuation in
            self.continuation = continuation
            self.manager = CBCentralManager(delegate: self, queue: nil)
            self.timeoutTask = Task { @MainActor [weak self] in
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                self?.finishIfNeeded()
            }
        }
    }

    nonisolated func centralManagerDidUpdateState(_ central: CBCentralManager) {
        Task { @MainActor in
            self.finishIfNeeded()
        }
    }

    private func finishIfNeeded() {
        guard let continuation else { return }
        self.continuation = nil
        timeoutTask?.cancel()
        timeoutTask = nil
        continuation.resume(returning: mapBluetoothAuthorization(CBCentralManager.authorization))
        manager = nil
    }

    private func mapBluetoothAuthorization(_ authorization: CBManagerAuthorization) -> PermissionStatus {
        switch authorization {
        case .allowedAlways:
            return .granted
        case .notDetermined, .denied:
            return .missing
        case .restricted:
            return .unknown
        @unknown default:
            return .unknown
        }
    }
}
#endif
