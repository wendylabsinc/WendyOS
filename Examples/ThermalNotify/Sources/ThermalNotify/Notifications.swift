import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SwiftProtobuf

typealias NotificationRequest = Wendy_System_V1_SendRequest

func notification(camera: VideoDevice, reading: ThermalReading) -> NotificationRequest {
    var request = NotificationRequest()
    request.audience.roles = [.owner, .admin, .billingManager, .member, .viewer]
    request.severity = .critical
    request.title = "Thermal camera above 80°C"
    // Bound camera names to leave room within the notification body's byte limit.
    let name = String(camera.name.prefix(120))
    request.body =
        "\(name) detected \(String(format: "%.2f", reading.celsius))°C at pixel (\(reading.x), \(reading.y)), above the 80°C threshold."
    request.deepLink = "wendy://devices/current/live"
    request.notificationID = UUID().uuidString.lowercased()
    request.metadata.fields = [
        "camera_id": .with { $0.numberValue = Double(camera.id) },
        "camera_stable_id": .with { $0.stringValue = camera.stableID },
        "camera_name": .with { $0.stringValue = camera.name },
        "max_celsius": .with { $0.numberValue = reading.celsius },
        "threshold_celsius": .with { $0.numberValue = 80 },
        "pixel_x": .with { $0.numberValue = Double(reading.x) },
        "pixel_y": .with { $0.numberValue = Double(reading.y) },
        "temperature_provenance": .with { $0.stringValue = reading.provenance },
    ]
    return request
}

struct NotificationSender: Sendable {
    let socketPath: String

    func send(_ request: NotificationRequest) async throws {
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: socketPath),
            transportSecurity: .plaintext
        )
        try await withGRPCClient(transport: transport) { client in
            let service = Wendy_System_V1_NotificationService.Client(wrapping: client)
            var options = CallOptions.defaults
            options.timeout = .seconds(20)
            _ = try await service.send(request, options: options)
        }
    }
}

// Retain the request UUID until accepted, including after an uncertain transport
// failure. Reusing an accepted UUID returns ALREADY_EXISTS, without a second push.
struct PendingAlert: Sendable {
    let cameraKey: String
    let request: NotificationRequest
}

struct AlertState {
    private var hot: Set<String> = []
    private var queue: [PendingAlert] = []
    private(set) var nextAttempt = Date.distantPast

    mutating func observe(camera: VideoDevice, reading: ThermalReading) {
        let key = camera.monitorKey
        // Conversion arithmetic can place exactly 80°C a few ULPs above 80.
        guard reading.celsius > 80 + 1e-9 else {
            hot.remove(key)
            return
        }
        guard hot.insert(key).inserted else { return }
        // Coalesce repeated excursions while this camera already has an unsent
        // alert. Capture the first event even if other cameras use the send slot.
        guard !queue.contains(where: { $0.cameraKey == key }) else { return }
        queue.append(.init(cameraKey: key, request: notification(camera: camera, reading: reading)))
    }

    mutating func next(now: Date) -> PendingAlert? {
        guard now >= nextAttempt, let alert = queue.first else { return nil }
        nextAttempt = now.addingTimeInterval(10)
        return alert
    }

    mutating func accepted(_ alert: PendingAlert, now: Date) {
        // Count from completion so a slow send cannot be followed immediately
        // by another queued alert. One shared cooldown covers all cameras.
        nextAttempt = max(nextAttempt, now.addingTimeInterval(10))
        queue.removeAll { $0.request.notificationID == alert.request.notificationID }
    }

    mutating func failed(now: Date, error: any Error) {
        var delay: TimeInterval = 60
        if let rpc = error as? RPCError, rpc.code == .resourceExhausted {
            // Avoid repeated refusals that can quarantine the entire app.
            delay = 900
            if let suffix = rpc.message.components(separatedBy: "retry after ").last,
                let token = suffix.split(whereSeparator: \.isWhitespace).first,
                let retry = ISO8601DateFormatter().date(from: String(token))
            {
                delay = max(delay, retry.timeIntervalSince(now) + 1)
            }
        }
        nextAttempt = max(nextAttempt, now.addingTimeInterval(delay))
    }
}

extension VideoDevice {
    var monitorKey: String { stableID.isEmpty ? "\(id):\(name)" : stableID }
}

actor AlertCoordinator {
    private var state = AlertState()
    private let sender: NotificationSender

    init(sender: NotificationSender) { self.sender = sender }

    func observe(camera: VideoDevice, reading: ThermalReading) {
        state.observe(camera: camera, reading: reading)
    }

    // A single sender drains the queue while camera readers keep processing
    // frames. A slow or unavailable Cloud connection cannot stall detection.
    func run() async throws {
        while !Task.isCancelled {
            if let alert = state.next(now: Date()) {
                do {
                    try await sender.send(alert.request)
                    state.accepted(alert, now: Date())
                    log(
                        "CRITICAL notification created: \(alert.request.notificationID), \(alert.request.body)"
                    )
                } catch let error as RPCError where error.code == .alreadyExists {
                    state.accepted(alert, now: Date())
                    log("Notification already accepted: \(alert.request.notificationID)")
                } catch {
                    if Task.isCancelled { return }
                    state.failed(now: Date(), error: error)
                    log(
                        "Notification \(alert.request.notificationID) failed: \(error). Next attempt after \(state.nextAttempt.ISO8601Format())"
                    )
                }
            }
            try await Task.sleep(for: .seconds(1))
        }
    }
}
