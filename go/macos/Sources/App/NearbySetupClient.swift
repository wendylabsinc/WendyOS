import Foundation

struct NearbySetupDevice: Identifiable, Sendable {
    let id, name: String
    var rssi: Int = 0
    var title: String { name.isEmpty ? "Wendy device" : DeviceDisplayName.title(name) }
}

struct NearbySetupSnapshot: Sendable {
    var session: UInt64 = 0
    var connected = false
    var lite = false
    var enrolled = false
    var attempted = false
    var pending = false
    var canResume = false
    var failure = ""
    var completed = false
    var wifiConnected = false
    var ssid = ""
    var deviceID = ""
    var networks: [String] = []
    var error: String?
}

enum NearbySetupAction: Int32, Sendable {
    case status, scanWiFi, connectWiFi, enroll, checkEnrollment
}

extension GoClient {
    func discoverSetupDevices() async throws -> [NearbySetupDevice] {
        try await perform {
            guard let p = WDNearbyDiscover() else { throw WendyError(message: "Could not discover devices.") }
            defer { WDNearbyDevicesFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                NearbySetupDevice(id: Self.string($0.address), name: Self.string($0.name), rssi: Int($0.rssi))
            }
        }
    }

    func openNearbySetup(_ address: String) async throws -> NearbySetupSnapshot {
        try Task.checkCancellation()
        // Opening creates a backend session. Receive its identity even if dismissal
        // cancels this task, so the newly opened BLE connection can be released.
        let result = try await perform(unlessCancelled: false) {
            try Self.withCString(address) { try Self.readNearbySetup(WDNearbyOpen($0)) }
        }
        if Task.isCancelled {
            await closeNearbySetup(result.session)
            throw CancellationError()
        }
        return result
    }

    func nearbySetupAction(_ action: NearbySetupAction, session: UInt64, ssid: String = "", password: String = "",
                           account: String = "", organization: String = "", name: String = "") async throws -> NearbySetupSnapshot {
        try await perform {
            try Self.withCString(ssid) { ssid in
                try Self.withCString(password) { password in
                    try Self.withCString(account) { account in
                        try Self.withCString(organization) { organization in
                            try Self.withCString(name) { name in
                                try Self.readNearbySetup(WDNearbyAction(session, action.rawValue, ssid, password, account, organization, name))
                            }
                        }
                    }
                }
            }
        }
    }

    func closeNearbySetup(_ session: UInt64) async {
        guard session != 0 else { return }
        _ = try? await perform(unlessCancelled: false) { WDNearbyClose(session) }
    }

    private static func readNearbySetup(_ p: UnsafeMutablePointer<WDNearbySetupResult>?) throws -> NearbySetupSnapshot {
        guard let p else { throw WendyError(message: "Missing setup response.") }
        defer { WDNearbySetupFree(p) }
        let s = p.pointee
        // Preserve attempted/pending even when the operation reports an error.
        return NearbySetupSnapshot(session: s.session, connected: s.connected != 0, lite: s.lite != 0,
            enrolled: s.enrolled != 0, attempted: s.attempted != 0, pending: s.pending != 0, canResume: s.can_resume != 0, failure: Self.string(s.failure),
            completed: s.completed != 0, wifiConnected: s.wifi_connected != 0, ssid: Self.string(s.ssid),
            deviceID: Self.string(s.device_id),
            networks: UnsafeBufferPointer(start: s.networks, count: s.network_count).map { Self.string($0) },
            error: s.status.message.map { String(cString: $0) })
    }
}

/// Only repeat reads and connection establishment. Enrollment and Wi-Fi writes
/// always require an explicit action, even after a lost reply.
enum NearbySetupObservation: Equatable {
    case idle, discover, reconnect, checkEnrollment, status, scanWiFi
}

extension NearbySetupSnapshot {
    func observation(hasSelection: Bool, wifiExpanded: Bool, statusAge: TimeInterval,
                     scanAge: TimeInterval) -> NearbySetupObservation {
        if completed { return .idle }
        if pending || attempted {
            // Keep setup usable while Cloud is pending. In particular, an offline
            // board must still accept Wi-Fi configuration without re-enrollment.
            if !enrolled {
                if (wifiExpanded || canResume) && !connected && statusAge >= 25 { return .reconnect }
                if connected && statusAge >= 25 { return .status }
                if connected && wifiExpanded && scanAge >= 20 { return .scanWiFi }
            }
            return pending ? .checkEnrollment : .idle
        }
        if enrolled || attempted { return .idle }
        if !hasSelection { return .discover }
        if !connected { return .reconnect }
        if statusAge >= 25 { return .status }
        if wifiExpanded && scanAge >= 20 { return .scanWiFi }
        return .idle
    }
}

/// Presentation choices never imply that Ethernet or enrollment was verified.
/// Only device observations advance to a successful enrollment result.
struct NearbySetupFlow {
    enum Step: Int, CaseIterable {
        case device = 1, network, organization, enroll
        var title: String {
            switch self {
            case .device: "Device"
            case .network: "Connection"
            case .organization: "Organization"
            case .enroll: "Enrollment"
            }
        }
    }
    private(set) var step: Step = .device
    mutating func choseDevice() { step = .network }
    mutating func continueFromNetwork(connected: Bool, useEthernet: Bool = false) {
        guard step == .network, connected || useEthernet else { return }
        step = .organization
    }
    mutating func backToNetwork() { if step == .organization { step = .network } }
    static func validWiFi(ssid: String, password: String) -> Bool {
        // These are protocol byte limits, not Swift character counts.
        !ssid.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && ssid.utf8.count <= 32 && password.utf8.count <= 64
    }
}
