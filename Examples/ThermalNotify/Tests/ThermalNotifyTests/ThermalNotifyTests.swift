import Foundation
import GRPCCore
import Testing

@testable import ThermalNotify

private let now = Date(timeIntervalSince1970: 1_800_000_000)

private func frame(_ values: [UInt16], scale: Double = 0.01) -> VideoFrame {
    var frame = VideoFrame()
    frame.codec = .raw
    frame.timestampNs = UInt64(now.timeIntervalSince1970 * 1_000_000_000)
    frame.data = Data(values.flatMap { [UInt8(truncatingIfNeeded: $0), UInt8($0 >> 8)] })
    frame.rawFormat.width = UInt32(values.count)
    frame.rawFormat.height = 1
    frame.rawFormat.bytesPerLine = UInt32(values.count * 2)
    frame.rawFormat.thermalPlane.version = 1
    frame.rawFormat.thermalPlane.width = UInt32(values.count)
    frame.rawFormat.thermalPlane.height = 1
    frame.rawFormat.thermalPlane.rowStrideBytes = UInt32(values.count * 2)
    frame.rawFormat.thermalPlane.invalidValues = [0, 65535]
    frame.rawFormat.thermalPlane.temperature.kelvinPerCount = scale
    frame.rawFormat.thermalPlane.temperature.provenance = "test conversion"
    return frame
}

private func camera(_ id: UInt32 = 0) -> VideoDevice {
    .with {
        $0.id = id
        $0.name = "Thermal camera"
        $0.stableID = "test:\(id)"
    }
}

private func reading(_ raw: UInt16) throws -> ThermalReading {
    try ThermalReading(frame: frame([raw]), now: now)
}

@Test func `only sensor pixels count, excluding preview, padding and invalid samples`() throws {
    var sample = frame([65000, 65000, 30000, 35316, 65000, 65000, 65535, 0, 65000, 65000])
    sample.rawFormat.thermalPlane.offsetBytes = 4
    sample.rawFormat.thermalPlane.width = 2
    sample.rawFormat.thermalPlane.height = 2
    sample.rawFormat.thermalPlane.rowStrideBytes = 8
    let result = try ThermalReading(frame: sample, now: now)
    #expect(abs(result.celsius - 80.01) < 0.000001)
    #expect(result.x == 1 && result.y == 0)
}

@Test func `reject counts-only, malformed, stale and entirely invalid frames`() throws {
    var sample = frame([35316])
    sample.rawFormat.thermalPlane.clearTemperature()
    #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
    sample = frame([35316])
    sample.rawFormat.thermalPlane.rowStrideBytes = 1
    #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
    sample = frame([35316])
    sample.rawFormat.thermalPlane.offsetBytes = UInt32.max
    #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
    #expect(throws: MonitorError.self) {
        try ThermalReading(frame: frame([35316]), now: now.addingTimeInterval(9))
    }
    #expect(throws: MonitorError.self) { try ThermalReading(frame: frame([0, 65535]), now: now) }
    #expect(throws: MonitorError.self) {
        try ThermalReading(frame: frame([35316], scale: .nan), now: now)
    }
}

@Test func `strict threshold, one alert per excursion, independent cameras and all roles`() throws {
    var state = AlertState()
    state.observe(camera: camera(), reading: try reading(35314))
    #expect(state.next(now: now) == nil)
    state.observe(camera: camera(), reading: try reading(35315))
    #expect(state.next(now: now) == nil)
    state.observe(camera: camera(), reading: try reading(35316))
    let alertCandidate = state.next(now: now)
    let alert = try #require(alertCandidate)
    let request = alert.request
    #expect(request.severity == .critical)
    #expect(request.audience.roles == [.owner, .admin, .billingManager, .member, .viewer])
    #expect(request.audience.userIds.isEmpty && request.audience.teamIds.isEmpty)
    #expect(request.deepLink == "wendy://devices/current/live")
    #expect(UUID(uuidString: request.notificationID) != nil)
    state.accepted(alert, now: now)
    state.observe(camera: camera(), reading: try reading(40000))
    #expect(state.next(now: now.addingTimeInterval(100)) == nil)
    state.observe(camera: camera(1), reading: try reading(40000))
    let otherCandidate = state.next(now: now.addingTimeInterval(100))
    let other = try #require(otherCandidate)
    #expect(other.cameraKey == camera(1).monitorKey)
    state.accepted(other, now: now.addingTimeInterval(100))
    state.observe(camera: camera(), reading: try reading(35315))
    state.observe(camera: camera(), reading: try reading(35316))
    let nextCandidate = state.next(now: now.addingTimeInterval(110))
    let next = try #require(nextCandidate)
    #expect(next.request.notificationID != request.notificationID)
}

@Test func `retry uses same ID with global backoff and rate-limit quarantine protection`() throws {
    var state = AlertState()
    state.observe(camera: camera(), reading: try reading(40000))
    let firstCandidate = state.next(now: now)
    let first = try #require(firstCandidate)
    state.observe(camera: camera(1), reading: try reading(40000))
    #expect(state.next(now: now.addingTimeInterval(9)) == nil)
    state.failed(now: now, error: RPCError(code: .unavailable, message: "offline"))
    #expect(state.next(now: now.addingTimeInterval(59)) == nil)
    let retryCandidate = state.next(now: now.addingTimeInterval(60))
    let retry = try #require(retryCandidate)
    #expect(retry.request.notificationID == first.request.notificationID)
    state.failed(
        now: now.addingTimeInterval(60),
        error: RPCError(code: .resourceExhausted, message: "retry later")
    )
    #expect(state.next(now: now.addingTimeInterval(959)) == nil)
    #expect(state.next(now: now.addingTimeInterval(960)) != nil)
}

@Test func `brief simultaneous excursions survive cooldown and cooling before delivery`() throws {
    var state = AlertState()
    for id: UInt32 in [0, 1] {
        state.observe(camera: camera(id), reading: try reading(40000))
        state.observe(camera: camera(id), reading: try reading(30000))
    }
    let firstCandidate = state.next(now: now)
    let first = try #require(firstCandidate)
    state.accepted(first, now: now)
    #expect(state.next(now: now.addingTimeInterval(9)) == nil)
    let secondCandidate = state.next(now: now.addingTimeInterval(10))
    let second = try #require(secondCandidate)
    #expect(first.cameraKey != second.cameraKey)
    state.accepted(second, now: now.addingTimeInterval(10))
    #expect(state.next(now: now.addingTimeInterval(20)) == nil)
}

@Test func `repeated excursions coalesce while a camera alert is pending`() throws {
    var state = AlertState()
    for _ in 0..<20 {
        state.observe(camera: camera(), reading: try reading(40000))
        state.observe(camera: camera(), reading: try reading(30000))
    }
    let alertCandidate = state.next(now: now)
    let alert = try #require(alertCandidate)
    state.accepted(alert, now: now)
    #expect(state.next(now: now.addingTimeInterval(10)) == nil)
}

private func hikFrame(hot: UInt16 = 5346, x: UInt16 = 1000, compact: Bool = false) -> VideoFrame {
    var sample = frame([1])
    let width = compact ? 96 : 256, height = compact ? 96 : 192
    let stride = width * 2, captureHeight = height * 2 + 8
    sample.data = Data(repeating: 0, count: stride * captureHeight)
    sample.rawFormat.fourcc = "YUYV"
    sample.rawFormat.width = UInt32(width)
    sample.rawFormat.height = UInt32(captureHeight)
    sample.rawFormat.bytesPerLine = UInt32(stride)
    sample.rawFormat.thermalPlane.adapter = "topdon-tc001-hik-v1"
    sample.rawFormat.thermalPlane.width = UInt32(width)
    sample.rawFormat.thermalPlane.height = UInt32(height)
    sample.rawFormat.thermalPlane.rowStrideBytes = UInt32(stride)
    sample.rawFormat.thermalPlane.clearTemperature()
    func put(_ offset: Int, _ value: UInt16) {
        sample.data[offset] = UInt8(truncatingIfNeeded: value)
        sample.data[offset + 1] = UInt8(value >> 8)
    }
    let header = (height + 1) * stride
    put(header, 0xccdd)
    put(header + 2, 0xaabb)
    put(header + 8, UInt16(width))
    put(header + 12, UInt16(height))
    let words: [UInt16] = compact
        ? [0xffff, 0xeeee, hot, x, 894, 4563, 136, 21, 4600, 4591]
        : [0xffff, 0xeeee, hot, x, 973, 4472, 74, 486, 4591, 4557]
    for (i, word) in words.enumerated() { put((captureHeight - 4) * stride + i * 2, word) }
    return sample
}

@Test func `96x96 TOPDON uses live camera summary temperatures and rejects bad geometry`() throws {
    var sample = hikFrame(hot: 5228, compact: true)
    sample.data.replaceSubrange(0..<(96 * 192), with: repeatElement(UInt8(254), count: 96 * 192))
    let reading = try ThermalReading(frame: sample, now: now)
    #expect(reading.celsius == 31.6875 && reading.x == 95 && reading.y == 85)
    for offset in [97*192, 97*192+8, 97*192+12, 196*192, 196*192+4, 196*192+10] {
        var bad = sample
        bad.data[offset] = 0; bad.data[offset+1] = 0
        #expect(throws: MonitorError.self) { try ThermalReading(frame: bad, now: now) }
    }
    sample.data.removeLast()
    #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
}

@Test func `96x96 TOPDON only fresh summaries can trigger the alert threshold`() throws {
    var decoder = ThermalFrameDecoder()
    var state = AlertState()
    #expect(throws: MonitorError.self) { try decoder.decode(hikFrame(hot: 8320, compact: true), now: now) }
    let reading = try decoder.decode(hikFrame(hot: 8321, compact: true), now: now.addingTimeInterval(0.4))
    state.observe(camera: camera(), reading: reading)
    let candidate = state.next(now: now)
    let alert = try #require(candidate)
    #expect(alert.request.metadata.fields["max_celsius"]?.numberValue == 80.015625)
    #expect(throws: MonitorError.self) { try decoder.decode(hikFrame(hot: 8321, compact: true), now: now.addingTimeInterval(3.4)) }
}

@Test func `HIK uses the camera maximum from metadata, not raw sensor pixels`() throws {
    var sample = hikFrame()
    // Fill the whole sensor plane with high counts. They must not become °C.
    sample.data.replaceSubrange(0..<(192 * 512), with: repeatElement(UInt8(254), count: 192 * 512))
    let reading = try ThermalReading(frame: sample, now: now)
    #expect(reading.celsius == 33.53125)
    #expect(reading.x == 255 && reading.y == 186)
    #expect(reading.provenance.contains("camera-reported maximum"))
}

@Test func `HIK discards startup and frozen summaries, accepting later updates`() throws {
    var decoder = ThermalFrameDecoder()
    let first = hikFrame(hot: 8321)
    #expect(throws: MonitorError.self) { try decoder.decode(first, now: now) }
    #expect(throws: MonitorError.self) {
        try decoder.decode(first, now: now.addingTimeInterval(0.4))
    }
    let fresh = hikFrame(hot: 8322)
    let reading = try decoder.decode(fresh, now: now.addingTimeInterval(0.8))
    #expect(reading.celsius == 80.03125)
    #expect(throws: MonitorError.self) {
        try decoder.decode(fresh, now: now.addingTimeInterval(3.8))
    }
    let renewed = try decoder.decode(hikFrame(hot: 8323), now: now.addingTimeInterval(4))
    #expect(renewed.celsius == 80.046875)
}

@Test func `HIK rejects corrupt, truncated and wrong-layout metadata`() throws {
    for offset in [193 * 512, 388 * 512, 388 * 512 + 4, 388 * 512 + 10] {
        var sample = hikFrame()
        sample.data[offset] = 0
        sample.data[offset + 1] = 0
        #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
    }
    #expect(throws: MonitorError.self) { try ThermalReading(frame: hikFrame(x: 1001), now: now) }
    var sample = hikFrame()
    sample.data.removeLast()
    #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
    sample = hikFrame()
    sample.rawFormat.height = 384
    #expect(throws: MonitorError.self) { try ThermalReading(frame: sample, now: now) }
}

@Test func `HIK triggers strictly above 80 Celsius and rearms at 80`() throws {
    var state = AlertState()
    state.observe(
        camera: camera(),
        reading: try ThermalReading(frame: hikFrame(hot: 8320), now: now)
    )
    #expect(state.next(now: now) == nil)
    state.observe(
        camera: camera(),
        reading: try ThermalReading(frame: hikFrame(hot: 8321), now: now)
    )
    let candidate = state.next(now: now)
    let alert = try #require(candidate)
    #expect(alert.request.severity == .critical)
    #expect(alert.request.metadata.fields["max_celsius"]?.numberValue == 80.015625)
    state.accepted(alert, now: now)
    state.observe(
        camera: camera(),
        reading: try ThermalReading(frame: hikFrame(hot: 8320), now: now)
    )
    state.observe(
        camera: camera(),
        reading: try ThermalReading(frame: hikFrame(hot: 8321), now: now)
    )
    #expect(state.next(now: now.addingTimeInterval(10)) != nil)
}

@Test func `ten second throttle starts at acceptance even after a slow send`() throws {
    var state = AlertState()
    state.observe(camera: camera(), reading: try reading(40000))
    state.observe(camera: camera(1), reading: try reading(40000))
    let candidate = state.next(now: now)
    let alert = try #require(candidate)
    state.accepted(alert, now: now.addingTimeInterval(20))
    #expect(state.next(now: now.addingTimeInterval(29.999)) == nil)
    #expect(state.next(now: now.addingTimeInterval(30)) != nil)
}

@Test func `custom 30 degree threshold controls alerts and payload`() throws {
    var state = AlertState()
    state.threshold = 30
    state.observe(camera: camera(), reading: try reading(30315))
    #expect(state.next(now: now) == nil)
    state.observe(camera: camera(), reading: try reading(30316))
    let candidate = state.next(now: now)
    let alert = try #require(candidate)
    #expect(alert.request.metadata.fields["threshold_celsius"]?.numberValue == 30)
    #expect(alert.request.title == "Thermal camera above 30°C")
    #expect(alert.request.body.contains("30°C threshold"))
}

@Test func `threshold defaults and invalid configuration`() throws {
    #expect(try thermalThreshold(nil) == 80)
    #expect(try thermalThreshold("30") == 30)
    #expect(try thermalThreshold("-5.5") == -5.5)
    for value in ["", "hot", "nan", "inf", "-inf"] {
        #expect(throws: MonitorError.self) { try thermalThreshold(value) }
    }
}

@Test func `test notification preserves real reading and explicit audience`() throws {
    let measured = try reading(29815)
    let recipient = UUID().uuidString.lowercased()
    let request = testNotificationRequest(camera: camera(), reading: measured, recipients: [recipient], threshold: 30)
    #expect(request.severity == .info)
    #expect(request.title == "Thermal notification test")
    #expect(request.body.contains("25.00°C"))
    #expect(request.body.contains("Test only"))
    #expect(request.audience.userIds == [recipient])
    #expect(request.audience.roles.isEmpty)
    #expect(request.metadata.fields["test"]?.boolValue == true)
    #expect(request.metadata.fields["max_celsius"]?.numberValue == measured.celsius)
    #expect(request.metadata.fields["threshold_celsius"]?.numberValue == 30)
}
