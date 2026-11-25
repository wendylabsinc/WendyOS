import Foundation

/// The authenticated Bluetooth management door of an enrolled device. The
/// device proved `deviceID`/`principal` with its enrolled certificate, and
/// accepted one of the account's operator certificates.
struct NearbyWiFiSnapshot: Sendable {
    var session: UInt64 = 0
    var connected = false
    var wifiConnected = false
    var ssid = ""
    var deviceID = ""
    var principal = ""
    var organization = ""
    var networks: [String] = []
    var error: String?
}

enum NearbyWiFiAction: Int32, Sendable {
    case status, scanWiFi, connectWiFi, disconnectWiFi, forgetWiFi
}

extension GoClient {
    func openNearbyWiFi(address: String, name: String, account: String) async throws -> NearbyWiFiSnapshot {
        try Task.checkCancellation()
        // Opening creates a backend session. Receive its identity even if dismissal
        // cancels this task, so the newly opened BLE connection can be released.
        let result = try await perform(unlessCancelled: false) {
            try Self.withCString(address) { address in
                try Self.withCString(name) { name in
                    try Self.withCString(account) { account in
                        try Self.readNearbyWiFi(WDNearbyWiFiOpen(address, name, account))
                    }
                }
            }
        }
        if Task.isCancelled {
            await closeNearbyWiFi(result.session)
            throw CancellationError()
        }
        return result
    }

    func nearbyWiFiAction(_ action: NearbyWiFiAction, session: UInt64, ssid: String = "", password: String = "") async throws -> NearbyWiFiSnapshot {
        try await perform {
            try Self.withCString(ssid) { ssid in
                try Self.withCString(password) { password in
                    try Self.readNearbyWiFi(WDNearbyWiFiAction(session, action.rawValue, ssid, password))
                }
            }
        }
    }

    func closeNearbyWiFi(_ session: UInt64) async {
        guard session != 0 else { return }
        _ = try? await perform(unlessCancelled: false) { WDNearbyWiFiClose(session) }
    }

    private static func readNearbyWiFi(_ p: UnsafeMutablePointer<WDNearbyWiFiResult>?) throws -> NearbyWiFiSnapshot {
        guard let p else { throw WendyError(message: "Missing Wi-Fi response.") }
        defer { WDNearbyWiFiFree(p) }
        let s = p.pointee
        // Preserve the session and connection state even when the operation reports an error.
        return NearbyWiFiSnapshot(session: s.session, connected: s.connected != 0, wifiConnected: s.wifi_connected != 0,
            ssid: Self.string(s.ssid), deviceID: Self.string(s.device_id), principal: Self.string(s.principal),
            organization: Self.string(s.organization),
            networks: UnsafeBufferPointer(start: s.networks, count: s.network_count).map { Self.string($0) },
            error: s.status.message.map { String(cString: $0) })
    }
}

/// Only repeat reads and connection establishment. Wi-Fi writes always
/// require an explicit action, even after a lost reply.
enum NearbyWiFiObservation: Equatable {
    case idle, reconnect, status, scanWiFi
}

extension NearbyWiFiSnapshot {
    func observation(canReconnect: Bool, statusAge: TimeInterval, scanAge: TimeInterval) -> NearbyWiFiObservation {
        if !connected { return canReconnect ? .reconnect : .idle }
        if statusAge >= 25 { return .status }
        if scanAge >= 20 { return .scanWiFi }
        return .idle
    }
}
