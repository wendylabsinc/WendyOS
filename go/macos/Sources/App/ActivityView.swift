import SwiftUI

private func cText(_ pointer: UnsafeMutablePointer<CChar>?) -> String { pointer.map { String(cString: $0) } ?? "" }
private func value(_ optional: WDOptionalDouble) -> Double? { optional.available == 0 ? nil : optional.value }
private func bytes(_ count: Int64) -> String { ByteCountFormatter.string(fromByteCount: max(0, count), countStyle: .memory) }
private func percent(_ value: Double?) -> String { value.map { String(format: "%.1f%%", $0) } ?? "—" }

struct ResourceSnapshot {
    struct GPU: Identifiable {
        let id: UInt32
        let name: String
        let utilization: Double
        let memoryUsed, memoryTotal: Int64
        let temperature, power: Double?
    }
    struct Thermal: Identifiable { let id: String; let temperature: Double }
    struct Battery { let percent: Double; let state: String; let seconds: Int64? }
    struct Row: Identifiable {
        let id, app, name: String
        let state: AppState
        let cpu: Double?
        let memory: Int64
        let isService: Bool
        var sortCPU: Double { cpu ?? -1 }
    }
    let hasHost: Bool
    let cpu: Double?
    let cpuCount: UInt32
    let memoryUsed, memoryTotal: Int64
    let hasDisk: Bool
    let diskMount, diskError: String
    let diskUsed, diskTotal: Int64
    let gpus: [GPU]
    let thermals: [Thermal]
    let battery: Battery?
    let rows: [Row]

    init(_ r: WDResourcesResult) {
        hasHost = r.has_host != 0; cpu = value(r.cpu); cpuCount = r.cpu_count
        memoryUsed = r.memory_used; memoryTotal = r.memory_total
        hasDisk = r.has_disk != 0; diskMount = cText(r.disk_mount); diskError = cText(r.disk_error)
        diskUsed = r.disk_used; diskTotal = r.disk_total
        gpus = UnsafeBufferPointer(start: r.gpus, count: r.gpu_count).map {
            GPU(id: $0.index, name: cText($0.name), utilization: $0.utilization, memoryUsed: $0.memory_used,
                memoryTotal: $0.memory_total, temperature: value($0.temperature), power: value($0.power))
        }
        thermals = UnsafeBufferPointer(start: r.thermals, count: r.thermal_count).map { Thermal(id: cText($0.name), temperature: $0.celsius) }
        if r.battery.available != 0 {
            let states = ["Unknown", "Charging", "On battery", "Full", "Not charging"]
            battery = Battery(percent: r.battery.percent,
                              state: states.indices.contains(Int(r.battery.state.rawValue)) ? states[Int(r.battery.state.rawValue)] : "Unknown",
                              seconds: r.battery.has_estimate != 0 ? r.battery.seconds_remaining : nil)
        } else { battery = nil }
        rows = UnsafeBufferPointer(start: r.rows, count: r.row_count).map {
            Row(id: cText($0.id), app: cText($0.app), name: cText($0.name), state: AppState($0.state),
                cpu: value($0.cpu), memory: $0.memory, isService: $0.is_service != 0)
        }
    }
}
struct ResourcePort: Identifiable {
    let protocolName, address: String
    let port: UInt32
    var id: String { protocolName + address + String(port) }
    var label: String { protocolName.uppercased() + "  " + (address.contains(":") ? "[" + address + "]" : address) + ":" + String(port) }
}

struct ActivityView: View {
    @ObservedObject var model: AppModel
    @State private var snapshot: ResourceSnapshot?
    @State private var error: String?
    @State private var live = true
    @State private var sortOrder = [KeyPathComparator(\ResourceSnapshot.Row.name, comparator: .localizedStandard)]
    @State private var selection: String?
    @State private var ports: [ResourcePort] = []
    @State private var portsError: String?
    @State private var lastUpdated: Date?

    private var selectedApp: String? { snapshot?.rows.first(where: { $0.id == selection })?.app }
    private var rows: [ResourceSnapshot.Row] {
        guard let rows = snapshot?.rows else { return [] }
        return rows.filter { !$0.isService }
            .sorted(using: sortOrder + [KeyPathComparator(\ResourceSnapshot.Row.name, comparator: .localizedStandard)])
            .flatMap { parent in
                [parent] + rows.filter { $0.isService && $0.app == parent.app }
                    .sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
            }
    }
    var body: some View {
        VStack(spacing: 8) {
            if let snapshot {
                HStack(alignment: .center, spacing: 16) {
                ScrollView(.horizontal) {
                    HStack(spacing: 12) {
                        metric("CPU", value: percent(snapshot.cpu), detail: "\(snapshot.cpuCount) logical cores",
                               fraction: (snapshot.cpu ?? 0) / 100, color: .blue)
                        metric("Memory", value: snapshot.hasHost ? bytes(snapshot.memoryUsed) : "—",
                               detail: snapshot.hasHost ? "of " + bytes(snapshot.memoryTotal) : "Unavailable",
                               fraction: ratio(snapshot.memoryUsed, snapshot.memoryTotal), color: .purple)
                        metric("Storage", value: snapshot.hasDisk ? bytes(snapshot.diskUsed) : "—",
                               detail: snapshot.hasDisk ? "of " + bytes(snapshot.diskTotal) + " · " + snapshot.diskMount : "Unavailable",
                               fraction: ratio(snapshot.diskUsed, snapshot.diskTotal), color: .orange)
                        ForEach(snapshot.gpus) { gpu in
                            metric("GPU · " + gpu.name, value: percent(gpu.utilization),
                                   detail: gpu.memoryTotal > 0 ? bytes(gpu.memoryUsed) + " / " + bytes(gpu.memoryTotal) : "Shared system memory",
                                   fraction: gpu.utilization / 100, color: .green)
                        }
                        if let battery = snapshot.battery {
                            metric("Battery", value: percent(battery.percent), detail: battery.state,
                                   fraction: battery.percent / 100, color: .green)
                        }
                    }
                }.scrollIndicators(.hidden)
                VStack(alignment: .trailing, spacing: 6) {
                    Toggle("Live", isOn: $live).toggleStyle(.switch).controlSize(.small).fixedSize()
                    if let lastUpdated { Text(lastUpdated, style: .time).font(.caption2.monospacedDigit()).foregroundStyle(.secondary) }
                }
                }
                if !snapshot.thermals.isEmpty || snapshot.gpus.contains(where: { $0.temperature != nil || $0.power != nil }) {
                    ScrollView(.horizontal) {
                        HStack(spacing: 16) {
                            ForEach(snapshot.thermals) { zone in
                                Label(zone.id + " " + String(format: "%.0f°C", zone.temperature), systemImage: "thermometer.medium")
                            }
                            ForEach(snapshot.gpus) { gpu in
                                if let temp = gpu.temperature { Text(gpu.name + " " + String(format: "%.0f°C", temp)) }
                                if let power = gpu.power { Text(gpu.name + " " + String(format: "%.1f W", power)) }
                            }
                        }.font(.caption).foregroundStyle(.secondary)
                    }.scrollIndicators(.hidden)
                }
                if let error { FailureNote(message: error) }
                if !snapshot.diskError.isEmpty { Text("Storage could not refresh. Other readings are current.").font(.caption).foregroundStyle(.secondary).help(snapshot.diskError) }
                HStack(alignment: .top, spacing: 16) {
                Table(rows, selection: $selection, sortOrder: $sortOrder) {
                    TableColumn("App / Service", value: \.name) { row in
                        HStack {
                            if row.isService { Image(systemName: "arrow.turn.down.right").foregroundStyle(.tertiary).padding(.leading, 16) }
                            Text(row.name).lineLimit(1).help(row.name)
                        }.frame(height: 30)
                    }.width(min: 200, ideal: 400)
                    TableColumn("CPU", value: \.sortCPU) { row in Text(percent(row.cpu)).monospacedDigit() }.width(85)
                    TableColumn("Memory", value: \.memory) { row in Text(row.state == .running ? bytes(row.memory) : "—").monospacedDigit() }.width(100)
                    TableColumn("Status") { row in
                        HStack(spacing: 8) {
                            Circle().fill(row.state.color).frame(width: 8, height: 8)
                            Text(row.state.title)
                        }
                    }.width(120)
                }
                .keyboardRowSelection(rows.map(\.id), selection: $selection)
                .contextMenu(forSelectionType: String.self) { ids in
                    if let id = ids.first, let row = snapshot.rows.first(where: { $0.id == id }) {
                        let state = snapshot.rows.first(where: { $0.id == row.app })?.state ?? row.state
                        AppControlMenu(model: model, app: row.app, state: state)
                        Divider()
                        AppDeleteMenu(model: model, app: row.app)
                    }
                }
                Divider()
                VStack(alignment: .leading, spacing: 12) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Listening ports").font(.subheadline.weight(.semibold))
                        if let selectedApp { Text(selectedApp).font(.caption).foregroundStyle(.secondary) }
                    }
                    if let portsError { FailureNote(message: portsError) }
                    else if selectedApp == nil { Text("Select an app to inspect its ports.").foregroundStyle(.secondary) }
                    else if ports.isEmpty { Text("No listening ports.").foregroundStyle(.secondary) }
                    else {
                        ScrollView {
                            VStack(alignment: .leading, spacing: 8) {
                                ForEach(ports) { port in
                                    VStack(alignment: .leading, spacing: 4) {
                                        Text(port.protocolName.uppercased()).font(.caption2.weight(.semibold)).foregroundStyle(.secondary)
                                        Text((port.address.contains(":") ? "[" + port.address + "]" : port.address) + ":" + String(port.port))
                                            .font(.caption.monospaced()).textSelection(.enabled)
                                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                                        .background(.quaternary, in: RoundedRectangle(cornerRadius: 8))
                                }
                            }
                        }
                    }
                }.frame(width: 230, alignment: .leading).padding(.top, 8)
                }
                Text("CPU percentages use the whole machine, across all cores.").font(.caption).foregroundStyle(.secondary)
            } else if let error {
                FailureView(title: "Could not load activity", error: error) { live = false; Task { await refresh() } }
            } else {
                ProgressView("Sampling device activity…").frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }.padding(.horizontal, 20).padding(.bottom, 10)
        .task(id: live) {
            guard live else { return }
            while !Task.isCancelled {
                await refresh()
                do { try await Task.sleep(for: .seconds(1)) } catch { return }
            }
        }
        .onChange(of: model.apps) { _, _ in Task { await refresh() } }
        .task(id: selectedApp) { await refreshPorts() }
    }

    private func ratio(_ used: Int64, _ total: Int64) -> Double { total > 0 ? min(1, max(0, Double(used) / Double(total))) : 0 }
    private func metric(_ title: String, value: String, detail: String, fraction: Double, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack {
                Text(title).font(.caption.weight(.medium)).foregroundStyle(.secondary).lineLimit(1)
                Spacer()
                Text(value).font(.headline).monospacedDigit()
            }
            ProgressView(value: min(1, max(0, fraction))).tint(color)
            Text(detail).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
        }.padding(8).frame(width: 210, alignment: .leading)
            .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 10))
    }
    private func refresh() async {
        do {
            let next = try await model.resources()
            guard !Task.isCancelled else { return }
            snapshot = next; error = nil; lastUpdated = Date()
            await refreshPorts()
        } catch {
            guard !Task.isCancelled else { return }
            self.error = error.localizedDescription
        }
    }
    private func refreshPorts() async {
        guard let app = selectedApp else { ports = []; portsError = nil; return }
        do {
            let result = try await model.ports(app: app)
            guard !Task.isCancelled, selectedApp == app else { return }
            ports = result; portsError = nil
        } catch {
            guard !Task.isCancelled, selectedApp == app else { return }
            portsError = error.localizedDescription
        }
    }
}
