import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2

func log(_ message: String) {
    // FileHandle avoids buffered stdout when running detached in a container.
    FileHandle.standardOutput.write(Data("\(Date().ISO8601Format()) \(message)\n".utf8))
}

func requiredSocket(_ key: String, entitlement: String) throws -> String {
    guard let path = ProcessInfo.processInfo.environment[key], !path.isEmpty,
        FileManager.default.fileExists(atPath: path)
    else {
        throw MonitorError(
            "\(key) is missing or unavailable. Run on WendyOS with the \(entitlement) entitlement."
        )
    }
    return path
}

@main
struct ThermalNotify {
    static func main() async throws {
        let delivery = ProcessInfo.processInfo.environment["THERMAL_NOTIFY_DELIVERY"] ?? "cloud"
        guard ["cloud", "monitor-only"].contains(delivery) else { throw MonitorError("THERMAL_NOTIFY_DELIVERY must be cloud or monitor-only") }
        let recipients = (ProcessInfo.processInfo.environment["THERMAL_NOTIFY_USER_IDS"] ?? "")
            .split(separator: ",").map { String($0).trimmingCharacters(in: .whitespaces) }
        guard recipients.allSatisfy({ UUID(uuidString: $0) != nil }) else { throw MonitorError("THERMAL_NOTIFY_USER_IDS must contain user UUIDs") }
        let sender: NotificationSender? = delivery == "cloud"
            ? NotificationSender(socketPath: try requiredSocket("WENDY_SYSTEM_SOCKET", entitlement: "notifications")) : nil
        let alerts = AlertCoordinator(sender: sender, recipients: recipients)
        if sender == nil { log("Monitor-only mode: no Cloud notifications will be sent.") }
        if let address = ProcessInfo.processInfo.environment["THERMAL_VIEWER_URL"] {
            guard let url = URL(string: address), url.scheme == "http", url.host == "127.0.0.1", url.port == 18842,
                url.path == "/api/frame", url.user == nil, url.password == nil, url.query == nil else {
                throw MonitorError("THERMAL_VIEWER_URL must be http://127.0.0.1:18842/api/frame on this device")
            }
            if CommandLine.arguments.contains("--test-notification") {
                guard let sender else { throw MonitorError("Cloud notification delivery is disabled") }
                guard !recipients.isEmpty else { throw MonitorError("A test must be addressed to explicit THERMAL_NOTIFY_USER_IDS") }
                let (_, reading) = try await ViewerSource.read(url)
                var request = notification(camera: ViewerSource.camera, reading: reading, recipients: recipients)
                request.title = "Thermal notification test"
                request.body = "Test only — this device's MLX90642 currently reads \(String(format: "%.2f", reading.celsius))°C at its hottest pixel. No high-temperature alarm."
                request.severity = .info
                request.metadata.fields["test"] = .with { $0.boolValue = true }
                try await sender.send(request)
                log("TEST notification accepted: \(request.notificationID)")
                return
            }
            log("Monitoring MLX90642 on this device; alerts above 80°C; recipients: \(recipients.isEmpty ? "organization roles" : "configured users").")
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask { try await alerts.run() }
                group.addTask { try await ViewerSource.monitor(url, alerts: alerts) }
                defer { group.cancelAll() }
                try await group.waitForAll()
            }
            return
        }
        let agentSocket = try requiredSocket("WENDY_AGENT_SOCKET", entitlement: "admin")
        let stableID = ProcessInfo.processInfo.environment["THERMAL_CAMERA_STABLE_ID"]
        log("Monitoring \(stableID ?? "all thermal cameras"); threshold above 80°C; delivery: \(delivery).")
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await alerts.run() }
            defer { group.cancelAll() }
            while !Task.isCancelled {
                do {
                    try await monitor(socket: agentSocket, alerts: alerts, stableID: stableID)
                } catch {
                    if Task.isCancelled { return }
                    log("Camera connection failed: \(error). Retrying in 5 seconds.")
                    try await Task.sleep(for: .seconds(5))
                }
            }
        }
    }

    static func monitor(socket: String, alerts: AlertCoordinator, stableID: String? = nil) async throws {
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: socket),
            transportSecurity: .plaintext
        )
        try await withGRPCClient(transport: transport) { client in
            let video = Wendy_Agent_Services_V1_WendyVideoService.Client(wrapping: client)
            try await withThrowingTaskGroup(of: Void.self) { group in
                var watching: Set<String> = []
                defer { group.cancelAll() }
                while !Task.isCancelled {
                    var options = CallOptions.defaults
                    options.timeout = .seconds(10)
                    let response = try await video.listVideoDevices(.init(), options: options)
                    let cameras = response.devices.filter { isThermalCamera($0) && (stableID == nil || $0.stableID == stableID) }
                    if cameras.isEmpty {
                        log("No thermal cameras found. Checking again in 5 seconds.")
                    }
                    // Discover new cameras without interrupting existing streams.
                    for camera in cameras where watching.insert(camera.monitorKey).inserted {
                        group.addTask {
                            while !Task.isCancelled {
                                var request = Wendy_Agent_Services_V1_StreamVideoRequest()
                                request.deviceID = camera.id
                                request.stableID = camera.stableID
                                request.codec = .raw
                                // Leave geometry and FPS at zero to join the existing
                                // capture. The Agent chooses the thermal adapter's
                                // default mode when no viewer has opened it yet.
                                do {
                                    try await video.streamVideo(request) { stream in
                                        var lastProblem: String?
                                        var reportedReading = false
                                        var lastReport = Date.distantPast
                                        var decoder = ThermalFrameDecoder()
                                        for try await frame in stream.messages {
                                            let reading: ThermalReading
                                            do {
                                                reading = try decoder.decode(frame)
                                            } catch {
                                                let problem = String(describing: error)
                                                if lastProblem != problem {
                                                    log(
                                                        "\(camera.name) [\(camera.monitorKey)]: \(problem)"
                                                    )
                                                    lastProblem = problem
                                                }
                                                continue
                                            }
                                            if !reportedReading || lastProblem != nil || Date().timeIntervalSince(lastReport) >= 30 {
                                                log(
                                                    "\(camera.name) [\(camera.monitorKey)]: maximum \(String(format: "%.2f", reading.celsius))°C, \(reading.provenance)"
                                                )
                                                reportedReading = true
                                                lastProblem = nil
                                                lastReport = .now
                                            }
                                            await alerts.observe(camera: camera, reading: reading)
                                        }
                                    }
                                } catch {
                                    if !Task.isCancelled { log("Camera \(camera.name): \(error)") }
                                }
                                try await Task.sleep(for: .seconds(5))
                            }
                        }
                    }
                    try await Task.sleep(for: .seconds(5))
                }

            }
        }
    }
}

func isThermalCamera(_ camera: VideoDevice) -> Bool {
    if camera.hasThermal { return true }
    // Older agents can identify a thermal camera without supplying calibrated
    // frames. Discover these too, then report the missing conversion explicitly.
    let name = camera.name.lowercased()
    return name.contains("purethermal") || name.contains("pure thermal") || name.contains("tc001")
}
