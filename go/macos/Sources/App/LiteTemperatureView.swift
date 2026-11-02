import SwiftUI

struct SensorTemperature: Sendable {
    let name: String
    let celsius: Double
    let timestampMicroseconds: UInt64
    let sequence: UInt32
}

extension GoClient {
    func sensorTemperature(session: UInt64) async throws -> SensorTemperature {
        try await perform {
            guard let pointer = WDSensorTemperature(session) else {
                throw WendyError(message: "Missing SensorLink temperature result.")
            }
            defer { WDSensorTemperatureFree(pointer) }
            let value = pointer.pointee
            try Self.check(value.status)
            return SensorTemperature(
                name: Self.string(value.name), celsius: value.celsius,
                timestampMicroseconds: value.timestamp_us, sequence: value.sequence
            )
        }
    }
}

struct LiteTemperatureView: View {
    let session: UInt64
    @State private var sample: SensorTemperature?
    @State private var received: Date?
    @State private var error: String?
    @State private var live = true
    @State private var retry = 0

    var body: some View {
        Form {
            Section("SensorLink temperature") {
                Toggle("Live readings", isOn: $live)
                if let sample, let received {
                    LabeledContent(sample.name.isEmpty ? "Temperature" : sample.name) {
                        Text(sample.celsius, format: .number.precision(.fractionLength(1))) + Text(" °C")
                    }
                    LabeledContent("Last received") { Text(received, style: .relative) }
                        .foregroundStyle(.secondary)
                } else if error == nil, live {
                    ProgressView("Waiting for a temperature sample…")
                }
                if let error { FailureNote(message: error) }
                Button("Retry") { retry += 1 }.disabled(!live)
            }
            Section {
                Text("Reads the device’s advertised SensorLink temperature channel over the selected connection. A reading is the source’s temperature, not the iPad or Mac’s.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }.formStyle(.grouped)
        .task(id: "\(session)/\(live)/\(retry)") {
            guard live else { return }
            do {
                while !Task.isCancelled {
                    let next = try await GoClient.shared.sensorTemperature(session: session)
                    try Task.checkCancellation()
                    sample = next
                    received = .now
                    error = nil
                    try await Task.sleep(for: .seconds(2))
                }
            } catch {
                if !Task.isCancelled { self.error = error.localizedDescription }
            }
        }
    }
}
