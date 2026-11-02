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
        let agentSocket = try requiredSocket("WENDY_AGENT_SOCKET", entitlement: "admin")
        let systemSocket = try requiredSocket("WENDY_SYSTEM_SOCKET", entitlement: "notifications")
        let alerts = AlertCoordinator(sender: .init(socketPath: systemSocket))
        log(
            "Monitoring all thermal cameras. CRITICAL alerts to every organization role when any valid pixel exceeds 80°C."
        )
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await alerts.run() }
            defer { group.cancelAll() }
            while !Task.isCancelled {
                do {
                    try await monitor(socket: agentSocket, alerts: alerts)
                } catch {
                    if Task.isCancelled { return }
                    log("Camera connection failed: \(error). Retrying in 5 seconds.")
                    try await Task.sleep(for: .seconds(5))
                }
            }
        }
    }

    static func monitor(socket: String, alerts: AlertCoordinator) async throws {
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
                    let cameras = response.devices.filter(isThermalCamera)
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
                                            if !reportedReading || lastProblem != nil {
                                                log(
                                                    "\(camera.name) [\(camera.monitorKey)]: maximum \(String(format: "%.2f", reading.celsius))°C, \(reading.provenance)"
                                                )
                                                reportedReading = true
                                                lastProblem = nil
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
