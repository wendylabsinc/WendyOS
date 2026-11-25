import SwiftUI

struct NetworkTrafficSnapshot {
    struct Row: Identifiable {
        let name: String
        let index: UInt32
        let received, sent: UInt64
        let receiveRate, sendRate: Double?
        let virtual, loopback, agent: Bool
        var id: String { agent ? "agent" : "\(index):\(name)" }
    }
    let rows: [Row]
    let interfacesError, agentError: String

    init(_ value: WDNetworkTrafficResult) {
        interfacesError = GoClient.string(value.interfaces_error)
        agentError = GoClient.string(value.agent_error)
        rows = UnsafeBufferPointer(start: value.rows, count: value.count).map {
            Row(name: GoClient.string($0.name), index: $0.index, received: $0.received_bytes, sent: $0.sent_bytes,
                receiveRate: $0.receive_rate.available != 0 ? $0.receive_rate.value : nil,
                sendRate: $0.send_rate.available != 0 ? $0.send_rate.value : nil,
                virtual: $0.virtual_interface != 0, loopback: $0.loopback != 0, agent: $0.agent != 0)
        }
    }
}

extension GoClient {
    func networkTraffic(session: UInt64) async throws -> NetworkTrafficSnapshot {
        try await perform {
            guard let p = WDNetworkTraffic(session) else { throw WendyError(message: "Missing network traffic statistics.") }
            defer { WDNetworkTrafficFree(p) }
            try Self.check(p.pointee.status)
            return NetworkTrafficSnapshot(p.pointee)
        }
    }
}

// Embedded in the shared macOS/iPhone Wi-Fi form. Only the lightweight network
// RPC is polled; no Wi-Fi scans, container inventory or GPU sampling.
struct NetworkTrafficSection: View {
    let session: UInt64
    @Environment(\.scenePhase) private var scenePhase
    @State private var snapshot: NetworkTrafficSnapshot?
    @State private var error: String?
    @State private var updated: Date?

    var body: some View {
        Section {
            if let snapshot {
                if let agent = snapshot.rows.first(where: \.agent) {
                    trafficRow(agent)
                } else {
                    LabeledContent("Wendy agent service", value: "Unavailable")
                }
                ForEach(snapshot.rows.filter { !$0.virtual && !$0.loopback && !$0.agent }) { row in
                    trafficRow(row)
                }
                if !snapshot.interfacesError.isEmpty {
                    Text(snapshot.interfacesError).foregroundStyle(.secondary)
                }
                let virtual = snapshot.rows.filter { $0.virtual || $0.loopback }
                if !virtual.isEmpty {
                    DisclosureGroup("Virtual and loopback interfaces") {
                        ForEach(virtual) { row in trafficRow(row) }
                        Text("These may count the same traffic as a physical interface. Do not add them together.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
            } else if error == nil {
                ProgressView("Reading network traffic…")
            }
            if let error {
                FailureNote(message: error)
                if snapshot != nil { Text("Showing the last received sample.").font(.caption).foregroundStyle(.secondary) }
            }
        } header: {
            Text("Network traffic")
        } footer: {
            VStack(alignment: .leading, spacing: 4) {
                Text("Received and sent from the device’s perspective. Interface totals include all apps and reset when the interface resets or the device restarts.")
                Text("Wendy agent service counts IP traffic, including its helpers, relayed traffic and loopback, since accounting began. App containers are excluded. Viewing statistics also generates traffic. This is not a cellular or internet usage meter.")
                if let updated { Text("Updated \(updated.formatted(date: .omitted, time: .standard))") }
            }
        }
        .task(id: "\(session):\(scenePhase == .active)") {
            guard scenePhase == .active else { return }
            snapshot = nil
            error = nil
            updated = nil
            while !Task.isCancelled {
                do {
                    let next = try await GoClient.shared.networkTraffic(session: session)
                    guard !Task.isCancelled else { return }
                    snapshot = next
                    updated = Date()
                    error = nil
                } catch {
                    guard !Task.isCancelled else { return }
                    self.error = error.localizedDescription
                }
                do { try await Task.sleep(for: .seconds(2)) } catch { return }
            }
        }
    }

    private func trafficRow(_ row: NetworkTrafficSnapshot.Row) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(row.name).font(.headline)
            LabeledContent("Receiving", value: rate(row.receiveRate))
            LabeledContent("Sending", value: rate(row.sendRate))
            LabeledContent("Total received", value: size(Double(row.received)))
            LabeledContent("Total sent", value: size(Double(row.sent)))
        }.monospacedDigit().padding(.vertical, 4)
    }

    private func rate(_ value: Double?) -> String { value.map { size($0) + "/s" } ?? "Measuring…" }
    private func size(_ value: Double) -> String {
        var amount = max(0, value)
        let units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"]
        var index = 0
        while amount >= 1024 && index < units.count - 1 { amount /= 1024; index += 1 }
        return String(format: index == 0 ? "%.0f %@" : "%.1f %@", amount, units[index])
    }
}
