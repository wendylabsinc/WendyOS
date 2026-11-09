import Foundation
import Testing
@testable import ThermalNotify

private func viewerFrame(age: Double = 0.1, width: Int = 32, status: String = "live") throws -> ViewerEnvelope {
    var pixels = Array(repeating: 22.0, count: 768)
    pixels[700] = 81
    let data = try JSONSerialization.data(withJSONObject: ["status": status, "age": age,
        "frame": ["width": width, "height": 24, "pixels": pixels, "seq": 2, "timestamp_us": 1_000_000, "number": 2]])
    return try JSONDecoder().decode(ViewerEnvelope.self, from: data)
}

@Test func viewerCelsiusTriggersOnlyTheConfiguredRecipient() throws {
    let (_, reading) = try viewerFrame().validated()
    #expect(reading.celsius == 81 && reading.x == 28 && reading.y == 21)
    var alerts = AlertState()
    alerts.recipients = ["test-user"]
    alerts.observe(camera: ViewerSource.camera, reading: reading)
    let next = alerts.next(now: .now)
    let alert = try #require(next)
    #expect(alert.request.audience.roles.isEmpty)
    #expect(alert.request.audience.userIds == ["test-user"])
}

@Test func viewerRejectsStaleAndWrongSensorData() throws {
    for frame in [try viewerFrame(age: 3), try viewerFrame(age: -1), try viewerFrame(width: 240), try viewerFrame(status: "disconnected")] {
        #expect(throws: MonitorError.self) { try frame.validated() }
    }
}
