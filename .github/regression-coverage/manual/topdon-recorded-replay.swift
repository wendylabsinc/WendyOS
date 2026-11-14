// Copy into a private ThermalNotify Tests/ThermalNotifyTests checkout to replay
// a reviewed physical capture. This file does not run in the ordinary suite.
// Set TOPDON_RECORDING to the capture directory and put reference.json there.
import CryptoKit
import Foundation
import SwiftProtobuf
import Testing

@testable import ThermalNotify

private struct RecordedManifest: Decodable {
    struct Frame: Decodable {
        let file: String
        let sha256: String
        let raw_sha256: String
        let received_ns: String
    }
    let stable_id: String
    let status: String
    let frames: [Frame]
}

private struct PhysicalReference: Decodable {
    struct Sample: Decodable {
        let frame: Int
        let expected_max_celsius: Double // Independently transcribed summary code / 64 - 50.
        let reference_celsius: Double // Simultaneous calibrated reference, not this camera.
        let tolerance_celsius: Double // Declared before capture from the reference/setup uncertainty.
        let x_min: Int
        let x_max: Int
        let y_min: Int
        let y_max: Int
    }
    let observer: String
    let reference_serial: String
    let calibration_record_sha256: String
    let samples: [Sample]
}

private func hash(_ data: Data) -> String {
    SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
}

@Test func recordedTopdonReplay() throws {
    let directory = try #require(ProcessInfo.processInfo.environment["TOPDON_RECORDING"])
    let root = URL(fileURLWithPath: directory, isDirectory: true)
    let manifest = try JSONDecoder().decode(RecordedManifest.self, from: Data(contentsOf: root.appendingPathComponent("manifest.json")))
    let reference = try JSONDecoder().decode(PhysicalReference.self, from: Data(contentsOf: root.appendingPathComponent("reference.json")))
    #expect(manifest.status == "captured")
    #expect(manifest.frames.count >= 3)
    #expect(!reference.observer.isEmpty && !reference.reference_serial.isEmpty)
    #expect(reference.calibration_record_sha256.count == 64)
    #expect(reference.samples.count >= 3)
    try #require(Set(reference.samples.map(\.frame)).count == reference.samples.count)
    let regions = reference.samples.map { "\($0.x_min),\($0.x_max),\($0.y_min),\($0.y_max)" }
    try #require(Set(regions).count >= 3, "Use independently observed left, center and right target regions")
    let camera = try VideoDevice(jsonUTF8Data: Data(contentsOf: root.appendingPathComponent("camera.json")))
    #expect(camera.stableID == manifest.stable_id)
    #expect(camera.thermal.adapter == "topdon-tc001-hik-v1")
    #expect(camera.thermal.captureWidth == 96 && camera.thermal.captureHeight == 200)
    var frames: [VideoFrame] = []
    var receipts: [Date] = []
    var decoder = ThermalFrameDecoder()
    var accepted: [(Int, ThermalReading)] = []
    var firstValidIndex: Int?
    for (index, entry) in manifest.frames.enumerated() {
        try #require(entry.file == String(format: "frame-%06d.json", index))
        let bytes = try Data(contentsOf: root.appendingPathComponent(entry.file))
        #expect(hash(bytes) == entry.sha256)
        let frame = try VideoFrame(jsonUTF8Data: bytes)
        #expect(hash(frame.data) == entry.raw_sha256)
        #expect(frame.data.count == 38400)
        let received = try #require(Double(entry.received_ns)) / 1_000_000_000
        let now = Date(timeIntervalSince1970: received)
        if let previous = receipts.last { try #require(now > previous, "Capture receipt times must be strictly ordered") }
        // Use archived receipt time, never Date.now or fabricated fresh timestamps.
        frames.append(frame)
        receipts.append(now)
        let measurement: ThermalReading
        do { measurement = try ThermalReading(frame: frame, now: now) }
        catch is MonitorError {
            // Real cameras may initially report uninitialized metadata. Keep it
            // in the recording and require the production decoder to reject it.
            #expect(throws: MonitorError.self) { try decoder.decode(frame, now: now) }
            continue
        }
        #expect(measurement.x >= 0 && measurement.x < 96 && measurement.y >= 0 && measurement.y < 96)
        if firstValidIndex == nil {
            firstValidIndex = index
            #expect(throws: MonitorError.self) { try decoder.decode(frame, now: now) }
        } else {
            do { accepted.append((index, try decoder.decode(frame, now: now))) }
            catch let error as MonitorError {
                #expect(error.description.contains("summary"))
            }
        }
    }
    #expect(!accepted.isEmpty, "A real summary must change after startup")
    for sample in reference.samples {
        try #require(sample.frame > 0 && sample.frame < frames.count)
        let frame = frames[sample.frame]
        let reading = try ThermalReading(frame: frame, now: receipts[sample.frame])
        #expect(accepted.contains { $0.0 == sample.frame }, "Reference samples must pass the actual freshness gate")
        #expect(sample.expected_max_celsius.isFinite && sample.reference_celsius.isFinite)
        #expect(sample.tolerance_celsius.isFinite && sample.tolerance_celsius > 0)
        #expect(abs(reading.celsius - sample.expected_max_celsius) < 0.000001)
        #expect(abs(reading.celsius - sample.reference_celsius) <= sample.tolerance_celsius)
        try #require(sample.x_min >= 0 && sample.x_max < 96 && sample.x_min <= sample.x_max)
        try #require(sample.y_min >= 0 && sample.y_max < 96 && sample.y_min <= sample.y_max)
        #expect((sample.x_min...sample.x_max).contains(reading.x))
        #expect((sample.y_min...sample.y_max).contains(reading.y))
        #expect(reading.provenance.contains("camera-reported maximum"))
    }
    let (index, reading) = try #require(accepted.first)
    let frame = frames[index], now = receipts[index]
    // The exact-boundary control changes only policy; original physical bytes stay unchanged.
    var state = AlertState()
    state.threshold = reading.celsius
    state.observe(camera: camera, reading: reading)
    #expect(state.next(now: now) == nil)
    state.threshold = reading.celsius - 1.0 / 64
    state.observe(camera: camera, reading: reading)
    let candidate = state.next(now: now)
    let event = try #require(candidate)
    #expect(event.request.metadata.fields["max_celsius"]?.numberValue == reading.celsius)
    #expect(event.request.metadata.fields["pixel_x"]?.numberValue == Double(reading.x))
    #expect(event.request.metadata.fields["pixel_y"]?.numberValue == Double(reading.y))
    state.accepted(event, now: now)
    state.observe(camera: camera, reading: reading)
    #expect(state.next(now: now.addingTimeInterval(10)) == nil)
    state.threshold = reading.celsius
    state.observe(camera: camera, reading: reading)
    state.threshold = reading.celsius - 1.0 / 64
    state.observe(camera: camera, reading: reading)
    #expect(state.next(now: now.addingTimeInterval(10)) != nil)

    #expect(throws: MonitorError.self) { try ThermalReading(frame: frame, now: Date(timeIntervalSince1970: Double(frame.timestampNs) / 1_000_000_000 + 6)) }
    // Derived negative controls are synthetic replays, never additional device recordings.
    var frozenDecoder = ThermalFrameDecoder()
    let firstValid = try #require(firstValidIndex)
    _ = try? frozenDecoder.decode(frames[firstValid], now: receipts[firstValid])
    _ = try frozenDecoder.decode(frame, now: now)
    var frozen = frame
    frozen.timestampNs = UInt64(now.addingTimeInterval(3.1).timeIntervalSince1970 * 1_000_000_000)
    #expect(throws: MonitorError.self) { try frozenDecoder.decode(frozen, now: now.addingTimeInterval(3.1)) }
    var badGeometry = frame
    badGeometry.data[97 * 192 + 8] = 0
    #expect(throws: MonitorError.self) { try ThermalReading(frame: badGeometry, now: now) }
    var badTrailer = frame
    badTrailer.data[196 * 192] = 0
    #expect(throws: MonitorError.self) { try ThermalReading(frame: badTrailer, now: now) }
    var truncated = frame
    truncated.data.removeLast()
    #expect(throws: MonitorError.self) { try ThermalReading(frame: truncated, now: now) }
}
