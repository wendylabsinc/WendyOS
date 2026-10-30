import Foundation

struct NearbySetupDevice: Identifiable, Sendable {
    let id, name: String
    var title: String { name.isEmpty ? "Wendy device" : name }
}

struct NearbySetupSnapshot: Sendable {
    var session: UInt64 = 0
    var connected = false
    var lite = false
    var enrolled = false
    var attempted = false
    var pending = false
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
                NearbySetupDevice(id: Self.string($0.address), name: Self.string($0.name))
            }
        }
    }

    func openNearbySetup(_ address: String) async throws -> NearbySetupSnapshot {
        try await perform { try Self.withCString(address) { try Self.readNearbySetup(WDNearbyOpen($0)) } }
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
            enrolled: s.enrolled != 0, attempted: s.attempted != 0, pending: s.pending != 0,
            completed: s.completed != 0, wifiConnected: s.wifi_connected != 0, ssid: Self.string(s.ssid),
            deviceID: Self.string(s.device_id),
            networks: UnsafeBufferPointer(start: s.networks, count: s.network_count).map { Self.string($0) },
            error: s.status.message.map { String(cString: $0) })
    }
}
