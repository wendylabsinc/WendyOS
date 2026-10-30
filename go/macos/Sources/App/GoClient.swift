import Foundation

struct WendyError: LocalizedError {
    let message: String
    var canForgetIdentity = false
    var errorDescription: String? { message }
}

// C calls stay off the UI thread. Every operation has a concrete signature;
// returned allocations are copied to Swift values before their typed destructor.
final class GoClient: Sendable {
    static let shared = GoClient()
    private let requests = DispatchQueue(label: "dev.wendy.desktop.requests", qos: .userInitiated)
    private let connections = DispatchQueue(label: "dev.wendy.desktop.connections", qos: .userInitiated)
    private let events = DispatchQueue(label: "dev.wendy.desktop.events", qos: .userInitiated)
    private let mobileLifecycle = DispatchQueue(label: "dev.wendy.mobile.lifecycle", qos: .userInitiated)

    func perform<T: Sendable>(unlessCancelled: Bool = true, _ work: @escaping @Sendable () throws -> T) async throws -> T {
        try await BlockingWork.run(on: requests, unlessCancelled: unlessCancelled, work)
    }
    static func check(_ status: WDStatus) throws {
        if let message = status.message {
            throw WendyError(message: String(cString: message), canForgetIdentity: status.can_forget_identity != 0)
        }
    }
    static func finish(_ pointer: UnsafeMutablePointer<WDStatus>?) throws {
        guard let pointer else { throw WendyError(message: "Missing Go status.") }
        defer { WDStatusFree(pointer) }
        try check(pointer.pointee)
    }
    static func string(_ pointer: UnsafeMutablePointer<CChar>?) -> String {
        pointer.map { String(cString: $0) } ?? ""
    }
    private static func optionalString(_ pointer: UnsafeMutablePointer<CChar>?) -> String? {
        let value = string(pointer)
        return value.isEmpty ? nil : value
    }
    static func withCString<T>(_ value: String, _ body: (UnsafeMutablePointer<CChar>) throws -> T) rethrows -> T {
        try value.withCString { try body(UnsafeMutablePointer(mutating: $0)) }
    }

    func account() async throws -> CloudAccount {
        try await perform {
            guard let pointer = WDAccount() else { throw WendyError(message: "Missing account result.") }
            defer { WDAccountFree(pointer) }
            try Self.check(pointer.pointee.status)
            return CloudAccount(state: pointer.pointee.state, context: Self.string(pointer.pointee.context), cloud: Self.string(pointer.pointee.cloud), email: Self.string(pointer.pointee.email))
        }
    }

    func accounts() async throws -> [SavedAccount] {
        try await perform {
            guard let p = WDAccounts() else { throw WendyError(message: "Missing accounts result.") }
            defer { WDAccountsFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                SavedAccount(account: CloudAccount(state: $0.state, context: Self.string($0.context), cloud: Self.string($0.cloud), email: Self.string($0.email)), active: $0.active != 0)
            }
        }
    }

    #if os(iOS)
        func initializeMobile(directory override: URL? = nil) async throws {
            let directory: URL
            if let override {
                directory = override
            } else {
                let legacy = try FileManager.default.url(
                    for: .applicationSupportDirectory, in: .userDomainMask,
                    appropriateFor: nil, create: true
                ).appendingPathComponent("Wendy", isDirectory: true)
                directory = FileManager.default.containerURL(
                    forSecurityApplicationGroupIdentifier: "group.dev.wendy.mobile"
                )?.appendingPathComponent("Wendy", isDirectory: true) ?? legacy
                if directory != legacy && !FileManager.default.fileExists(atPath: directory.path)
                    && FileManager.default.fileExists(atPath: legacy.path) {
                    try FileManager.default.copyItem(at: legacy, to: directory)
                }
            }
            try await perform {
                try Self.withCString(directory.path) { try Self.finish(WDInitializeMobile($0)) }
            }
        }
        func setMobileState(active: Bool, account: String, organization: String) async throws {
            try await BlockingWork.run(on: mobileLifecycle, unlessCancelled: false) {
                try Self.withCString(account) { account in
                    try Self.withCString(organization) { organization in
                        try Self.finish(WDSetMobileState(active ? 1 : 0, account, organization))
                    }
                }
            }
        }
        func mobileMeshSummary() async throws -> (String, String) {
            try await perform {
                guard let result = WDMobileMeshSummary() else {
                    throw WendyError(message: "Missing mesh status.")
                }
                defer { WDMeshSummaryFree(result) }
                return (Self.string(result.pointee.state), Self.string(result.pointee.detail))
            }
        }
    #endif

    func initialize(cliPath: String) async throws {
        try await perform { try Self.withCString(cliPath) { try Self.finish(WDInitialize($0)) } }
    }
    func nearby() async throws -> [Device] { try await perform { try Self.readDevices(WDNearby()) } }
    func cloud() async throws -> [Device] { try await perform { try Self.readDevices(WDCloud()) } }
    func login(development: Bool) async throws { try await perform { try Self.finish(WDLogin(development ? 1 : 0)) } }
    func cancelLogin() async throws { try await perform(unlessCancelled: false) { try Self.finish(WDCancelLogin()) } }
    func beginConnection() -> UInt64 { WDBeginConnection() }
    func connect(selector: String, request: UInt64, forgetIdentity: Bool = false) async throws -> DeviceConnection {
        try await BlockingWork.run(on: connections) {
            try Self.withCString(selector) {
                try Self.readConnection(forgetIdentity ? WDForgetIdentityAndReconnect($0, request) : WDConnect($0, request))
            }
        }
    }

    func disconnect() async throws { try await perform(unlessCancelled: false) { try Self.finish(WDDisconnect()) } }
    func apps() async throws -> [DeviceApp] { try await perform { try Self.readApps(WDApps()) } }
    func openApp(_ app: String, session: UInt64) async throws -> URL {
        try await perform {
            try Self.withCString(app) {
                guard let result = WDOpenApp($0, session) else { throw WendyError(message: "Missing app URL result.") }
                defer { WDOpenAppFree(result) }
                try Self.check(result.pointee.status)
                guard let url = URL(string: Self.string(result.pointee.url)), url.scheme == "http", url.host != nil else {
                    throw WendyError(message: "The app has no valid webpage address.")
                }
                return url
            }
        }
    }
    func startApp(_ app: String, session: UInt64) async throws {
        try await perform { try Self.withCString(app) { try Self.finish(WDStartApp($0, session)) } }
    }
    func stopApp(_ app: String, session: UInt64) async throws {
        try await perform { try Self.withCString(app) { try Self.finish(WDStopApp($0, session)) } }
    }
    func deleteApp(_ app: String, session: UInt64) async throws {
        try await perform { try Self.withCString(app) { try Self.finish(WDDeleteApp($0, session)) } }
    }
    func cameras() async throws -> [Camera] { try await perform { try Self.readCameras(WDCameras()) } }
    func startLogs(app: String, stream: UInt64) async throws {
        try await perform { try Self.withCString(app) { try Self.finish(WDStartLogs($0, stream)) } }
    }
    func stopLogs() async throws { try await perform(unlessCancelled: false) { try Self.finish(WDStopLogs()) } }
    func startCamera(id: UInt32, stream: UInt64, session: UInt64, stableID: String = "") async throws {
        try await perform { try Self.withCString(stableID) { try Self.finish(WDStartCamera(id, stream, session, $0)) } }
    }
    func stopCamera(id: UInt32, stream: UInt64, session: UInt64) async throws {
        try await perform(unlessCancelled: false) { try Self.finish(WDStopCamera(id, stream, session)) }
    }
    func stopCameras(session: UInt64) async throws { try await perform(unlessCancelled: false) { try Self.finish(WDStopCameras(session)) } }
    func startVPN(routes: [VPNRouteSelection], gateway: String = "") async throws -> VPNConfiguration {
        try await perform {
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            var inputs = routes.map { WDRouteInput(selector: strings.add($0.selector), name: strings.add($0.name)) }
            return try inputs.withUnsafeMutableBufferPointer { try Self.readVPN(WDStartVPN($0.baseAddress, $0.count, strings.add(gateway))) }
        }
    }
    func stopVPN() async throws { try await perform(unlessCancelled: false) { try Self.finish(WDStopVPN()) } }
    func shutdown() async throws { try await perform(unlessCancelled: false) { try Self.finish(WDShutdown()) } }

    func resources(session: UInt64) async throws -> ResourceSnapshot {
        try await perform {
            guard let p = WDResources(session) else { throw WendyError(message: "Missing resource result.") }
            defer { WDResourcesFree(p) }
            try Self.check(p.pointee.status)
            return ResourceSnapshot(p.pointee)
        }
    }
    func ports(app: String, session: UInt64) async throws -> [ResourcePort] {
        try await perform {
            try Self.withCString(app) {
                guard let p = WDPorts($0, session) else { throw WendyError(message: "Missing ports result.") }
                defer { WDPortsFree(p) }
                try Self.check(p.pointee.status)
                return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                    ResourcePort(protocolName: Self.string($0.protocol), address: Self.string($0.address), port: $0.port)
                }
            }
        }
    }

    func poll() async throws -> [BridgeEvent] {
        try await BlockingWork.run(on: events) { try Self.readEvents(WDPollEvents()) }
    }

    private static func readConnection(_ pointer: UnsafeMutablePointer<WDConnectionResult>?) throws -> DeviceConnection {
        guard let pointer else { throw WendyError(message: "Missing connection result.") }
        defer { WDConnectionFree(pointer) }
        try check(pointer.pointee.status)
        return DeviceConnection(session: pointer.pointee.session, lite: pointer.pointee.lite != 0)
    }
    static func readDevices(_ pointer: UnsafeMutablePointer<WDDevicesResult>?) throws -> [Device] {
        guard let pointer else { throw WendyError(message: "Missing devices result.") }
        defer { WDDevicesFree(pointer) }
        try check(pointer.pointee.status)
        return UnsafeBufferPointer(start: pointer.pointee.items, count: pointer.pointee.count).map {
            Device(id: string($0.id), name: string($0.name), selector: string($0.selector),
                   transport: DeviceTransport($0.transport), address: string($0.address), lite: $0.lite != 0)
        }
    }
    static func readApps(_ pointer: UnsafeMutablePointer<WDAppsResult>?) throws -> [DeviceApp] {
        guard let pointer else { throw WendyError(message: "Missing apps result.") }
        defer { WDAppsFree(pointer) }
        try check(pointer.pointee.status)
        return UnsafeBufferPointer(start: pointer.pointee.items, count: pointer.pointee.count).map {
            DeviceApp(appName: string($0.name), appVersion: optionalString($0.version),
                      runningState: AppState($0.state),
                      services: UnsafeBufferPointer(start: $0.services, count: $0.service_count).map {
                          DeviceApp.Service(name: string($0.name), state: AppState($0.state))
                      }, terminationReason: optionalString($0.termination_reason), httpPort: $0.http_port)
        }
    }
    static func readCameras(_ pointer: UnsafeMutablePointer<WDCamerasResult>?) throws -> [Camera] {
        guard let pointer else { throw WendyError(message: "Missing cameras result.") }
        defer { WDCamerasFree(pointer) }
        try check(pointer.pointee.status)
        return UnsafeBufferPointer(start: pointer.pointee.items, count: pointer.pointee.count).map {
            Camera(id: $0.id, name: string($0.name), path: optionalString($0.path), thermal: $0.thermal != 0, stableID: string($0.stable_id))
        }
    }
    private static func readVPN(_ pointer: UnsafeMutablePointer<WDVPNResult>?) throws -> VPNConfiguration {
        guard let pointer else { throw WendyError(message: "Missing VPN result.") }
        defer { WDVPNFree(pointer) }
        try check(pointer.pointee.status)
        let config = pointer.pointee.config
        return VPNConfiguration(endpoint: string(config.endpoint), token: string(config.token),
                                routes: UnsafeBufferPointer(start: config.routes, count: config.route_count).map {
            VPNRoute(selector: string($0.selector), name: string($0.name), address: string($0.address), host: string($0.host))
        }, internet: string(config.internet_address4).isEmpty ? nil : VPNInternetConfiguration(
            address4: string(config.internet_address4), address6: string(config.internet_address6), mtu: Int(config.internet_mtu)),
           bootstrapDirectory: string(config.bootstrap_directory), gateway: string(config.gateway))
    }
    static func readEvents(_ pointer: UnsafeMutablePointer<WDEvents>?) throws -> [BridgeEvent] {
        guard let pointer else { throw WendyError(message: "Missing events result.") }
        defer { WDEventsFree(pointer) }
        return try UnsafeBufferPointer(start: pointer.pointee.items, count: pointer.pointee.count).map {
            let payload: BridgeEvent.Payload
            switch $0.kind {
            case WD_EVENT_LOGIN_URL: payload = .loginURL(string($0.text))
            case WD_EVENT_LOGIN_DONE: payload = .loginDone
            case WD_EVENT_LOGIN_ERROR: payload = .loginError(string($0.text))
            case WD_EVENT_VPN_ERROR: payload = .vpnError(string($0.text))
            case WD_EVENT_LOG:
                payload = .log(LogEntry(timestampNanoseconds: $0.log.timestamp_ns,
                                       level: LogLevel($0.log.level), body: string($0.log.body),
                                       app: string($0.log.app), service: string($0.log.service)))
            case WD_EVENT_LOGS_ERROR: payload = .logsError(string($0.text))
            case WD_EVENT_CAMERA_ERROR: payload = .cameraError(string($0.text))
            case WD_EVENT_VIDEO:
                if $0.byte_count == 0 { payload = .video(Data()) }
                else if let bytes = $0.bytes { payload = .video(Data(bytes: bytes, count: $0.byte_count)) }
                else { throw WendyError(message: "Video event has no byte buffer.") }
            case WD_EVENT_THERMAL:
                payload = .thermal(try ThermalCameraFrame($0.thermal, bytes: $0.bytes, count: $0.byte_count))
            case WD_EVENT_JPEG:
                guard let bytes = $0.bytes, $0.byte_count > 0 else { throw WendyError(message: "JPEG event has no byte buffer.") }
                payload = .jpeg(Data(bytes: bytes, count: $0.byte_count))
            default: throw WendyError(message: "Unrecognized Go event type.")
            }
            return BridgeEvent(session: $0.session, stream: $0.stream, payload: payload)
        }
    }
}

struct BridgeEvent {
    enum Payload {
        case loginURL(String), loginDone, loginError(String)
        case log(LogEntry), logsError(String), video(Data), jpeg(Data), cameraError(String)
        case thermal(ThermalCameraFrame)
        case vpnError(String)
    }
    let session: UInt64
    let stream: UInt64
    let payload: Payload
}
enum DeviceTransport: String {
    case lan = "LAN", mesh = "Mesh", cloud = "Cloud", usb = "USB"
    init(_ value: WDTransport) {
        switch value {
        case WD_TRANSPORT_MESH: self = .mesh
        case WD_TRANSPORT_CLOUD: self = .cloud
        case WD_TRANSPORT_USB: self = .usb
        default: self = .lan
        }
    }
}
enum AppState: String {
    case stopped = "STOPPED", running = "RUNNING", crashLooping = "CRASH_LOOPING", unknown = "UNKNOWN"
    init(_ value: WDAppState) {
        switch value {
        case WD_APP_STOPPED: self = .stopped
        case WD_APP_RUNNING: self = .running
        case WD_APP_CRASH_LOOPING: self = .crashLooping
        default: self = .unknown
        }
    }
}
struct DeviceConnection { let session: UInt64; let lite: Bool }

struct Device: Identifiable, Hashable {
    let id: String
    let name: String
    let selector: String
    let transport: DeviceTransport
    let address: String
    var lite = false
    var routable: Bool { !lite && (transport == .mesh || transport == .cloud) }
    var title: String { name.isEmpty ? selector : name }
}
struct DeviceApp: Identifiable, Equatable {
    let appName: String
    var appVersion: String?
    var runningState: AppState
    var services: [Service]
    var terminationReason: String?
    var httpPort: UInt32 = 0
    var hasWebpage: Bool { (1...65535).contains(httpPort) }
    var id: String { appName }
    var state: String { runningState.rawValue }
    struct Service: Identifiable, Equatable {
        var name: String
        var state: AppState
        var id: String { name }
    }
}
struct Camera: Identifiable, Hashable {
    let id: UInt32
    let name: String
    var path: String?
    var thermal = false
    var stableID = ""
}
struct VPNRouteSelection { let selector: String; let name: String }

struct CloudAccount: Equatable {
    let state: WDAccountState
    let context: String
    let cloud: String
    let email: String
    var signedIn: Bool { state == WD_ACCOUNT_SIGNED_IN }
    var title: String {
        if state == WD_ACCOUNT_NEEDS_LOGIN { return "Sign in again" }
        return signedIn ? "Signed in" : "Not signed in"
    }
}

struct SavedAccount: Identifiable {
    let account: CloudAccount
    let active: Bool
    var id: String { account.context }
}
