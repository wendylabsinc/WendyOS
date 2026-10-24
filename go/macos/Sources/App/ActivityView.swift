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
    @State private var visible = true
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
        Group {
            #if os(iOS)
            mobileContent
            #else
            desktopContent
            #endif
        }
        .onAppear { visible = true }.onDisappear { visible = false }
        .task(id: live && visible) {
            guard live && visible else { return }
            while !Task.isCancelled {
                await refresh()
                do { try await Task.sleep(for: .seconds(1)) } catch { return }
            }
        }
        .task(id: model.apps) { await refresh() }
        .task(id: selectedApp) { await refreshPorts() }
    }

    private var desktopContent: some View {
        VStack(spacing: 0) {
            if let snapshot {
                VStack(spacing: 8) {
                    #if os(iOS)
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 145), spacing: 8)], spacing: 8) {
                        metrics(snapshot)
                    }
                    if !snapshot.thermals.isEmpty || snapshot.gpus.contains(where: { $0.temperature != nil || $0.power != nil }) {
                        DisclosureGroup("Temperature & power") {
                            ScrollView {
                                VStack(alignment: .leading, spacing: 8) {
                                    ForEach(snapshot.thermals) { zone in
                                        HStack {
                                            Text(zone.id).lineLimit(1)
                                            Spacer()
                                            Text(String(format: "%.0f°C", zone.temperature)).monospacedDigit()
                                        }
                                    }
                                    ForEach(snapshot.gpus) { gpu in
                                        if let temp = gpu.temperature { Text(gpu.name + " · " + String(format: "%.0f°C", temp)) }
                                        if let power = gpu.power { Text(gpu.name + " · " + String(format: "%.1f W", power)) }
                                    }
                                }.font(.caption).foregroundStyle(.secondary).padding(.top, 8)
                            }.frame(maxHeight: 120)
                        }.font(.caption).padding(.horizontal, 8)
                    }
                    #else
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
                    #endif
                    if let error { FailureNote(message: error) }
                    if !snapshot.diskError.isEmpty { Text("Storage could not refresh. Other readings are current.").font(.caption).foregroundStyle(.secondary).help(snapshot.diskError) }
                }.padding(.horizontal, 20).padding(.vertical, 12)
                #if os(iOS)
                List(rows) { row in
                    DisclosureGroup(isExpanded: Binding(get: { selection == row.id }, set: { expanded in
                        ports = []; portsError = nil
                        selection = expanded ? row.id : nil
                    })) {
                        if let portsError { FailureNote(message: portsError) }
                        ForEach(ports) { port in Text("\(port.protocolName.uppercased()) \(port.address):\(port.port)").font(.caption.monospaced()) }
                        AppControlMenu(model: model, app: row.app, state: row.state)
                    } label: {
                        VStack(alignment: .leading, spacing: 6) {
                            Text(row.name).font(.subheadline.weight(.medium))
                            HStack {
                                Text("CPU " + percent(row.cpu))
                                Text(bytes(row.memory))
                                Spacer()
                                Text(row.state.title).foregroundStyle(row.state.color)
                            }.font(.caption).monospacedDigit()
                        }.padding(.vertical, 4)
                    }
                }.listStyle(.plain).refreshable { await refresh() }
                #else
                HStack(alignment: .top, spacing: 0) {
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
                .tableStyle(.bordered)
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
                }.frame(width: 230, alignment: .leading).padding(16)
                }
                #endif
                Text("CPU percentages use the whole machine, across all cores.").font(.caption).foregroundStyle(.secondary).padding(.vertical, 8)
            } else if let error {
                FailureView(title: "Could not load activity", error: error) { live = false; Task { await refresh() } }
            } else {
                ProgressView("Sampling device activity…").frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
    }
    #if os(iOS)
    private var mobileContent: some View {
        List {
            Section("Device") {
                if let snapshot {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 140), spacing: 8)], spacing: 12) { metrics(snapshot) }
                        .listRowInsets(EdgeInsets(top: 8, leading: 8, bottom: 8, trailing: 8))
                    if !snapshot.thermals.isEmpty || snapshot.gpus.contains(where: { $0.temperature != nil || $0.power != nil }) {
                        NavigationLink("Temperature & power") {
                            Form {
                                Section("Temperature") {
                                    ForEach(snapshot.thermals) { zone in LabeledContent(zone.id, value: String(format: "%.0f°C", zone.temperature)) }
                                }
                                ForEach(snapshot.gpus) { gpu in
                                    Section(gpu.name) {
                                        if let temp = gpu.temperature { LabeledContent("Temperature", value: String(format: "%.0f°C", temp)) }
                                        if let power = gpu.power { LabeledContent("Power", value: String(format: "%.1f W", power)) }
                                    }
                                }
                            }.navigationTitle("Temperature & power").navigationBarTitleDisplayMode(.inline)
                        }
                    }
                } else if error == nil { HStack { ProgressView(); Text("Sampling activity…").foregroundStyle(.secondary) } }
                if let error { FailureNote(message: error) }
                if let snapshot, !snapshot.diskError.isEmpty { Text("Storage is unavailable.").font(.caption).foregroundStyle(.secondary) }
            }
            Section("Apps") {
                if let error = model.appsError { FailureNote(message: error) }
                ForEach(model.apps) { app in
                    NavigationLink {
                        AppActivityDetail(model: model, appName: app.appName, initialSnapshot: snapshot)
                    } label: {
                        VStack(alignment: .leading, spacing: 7) {
                            Text(app.appName).font(.subheadline.weight(.semibold)).lineLimit(2)
                            HStack(spacing: 6) {
                                Circle().fill(app.runningState.color).frame(width: 6, height: 6)
                                Text(app.runningState.title)
                                if let row = snapshot?.rows.first(where: { !$0.isService && $0.app == app.appName }), app.runningState == .running {
                                    Text("· CPU " + percent(row.cpu))
                                    Text("· " + bytes(row.memory))
                                } else if let version = app.appVersion { Text("· " + version) }
                            }.font(.caption).foregroundStyle(.secondary).monospacedDigit()
                        }.padding(.vertical, 5)
                    }
                }
                if model.apps.isEmpty && model.appsError == nil { Text("No apps installed").foregroundStyle(.secondary) }
            }
        }.listStyle(.insetGrouped)
            .refreshable { await model.refreshDetails(); await refresh() }
    }
    #endif

    @ViewBuilder private func metrics(_ snapshot: ResourceSnapshot) -> some View {
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
        }.padding(8).desktopFrame(width: 210)
            .background(PlatformColors.control, in: RoundedRectangle(cornerRadius: 10))
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

#if os(iOS)
private struct AppActivityDetail: View {
    @ObservedObject var model: AppModel
    let appName: String
    let initialSnapshot: ResourceSnapshot?
    @State private var snapshot: ResourceSnapshot?
    @State private var ports: [ResourcePort] = []
    @State private var error: String?
    @State private var portError: String?
    @State private var loadingPorts = true
    @State private var visible = true
    @State private var showingLogs = false
    private var app: DeviceApp? { model.apps.first { $0.appName == appName } }
    private var resource: ResourceSnapshot.Row? { (snapshot ?? initialSnapshot)?.rows.first { !$0.isService && $0.app == appName } }
    var body: some View {
        Form {
            Section {
                Text(appName).font(.headline).textSelection(.enabled)
                if let app {
                    LabeledContent("Status") { Text(app.runningState.title).foregroundStyle(app.runningState.color) }
                    if let version = app.appVersion { LabeledContent("Version", value: version) }
                    if let reason = app.terminationReason, !reason.isEmpty { Text(reason).font(.callout).foregroundStyle(.secondary) }
                }
            }
            Section("Usage") {
                if let error { FailureNote(message: error) }
                LabeledContent("CPU", value: percent(resource?.cpu))
                LabeledContent("Memory", value: resource.map { bytes($0.memory) } ?? "Unavailable")
                LabeledContent("App GPU", value: "Not reported")
            }
            if let data = snapshot ?? initialSnapshot, !data.gpus.isEmpty {
                Section("Device GPU") {
                    ForEach(data.gpus) { gpu in
                        LabeledContent(gpu.name, value: percent(gpu.utilization))
                        if gpu.memoryTotal > 0 { LabeledContent("GPU memory", value: bytes(gpu.memoryUsed) + " / " + bytes(gpu.memoryTotal)) }
                        else { Text("Shared system memory").foregroundStyle(.secondary) }
                    }
                }
            }
            Section("Ports") {
                if let portError { FailureNote(message: portError) }
                else if loadingPorts && ports.isEmpty { ProgressView() }
                else if ports.isEmpty { Text("No listening ports").foregroundStyle(.secondary) }
                ForEach(ports) { port in Text(port.label).font(.callout.monospaced()).textSelection(.enabled) }
            }
            if let app {
                Section {
                    if app.hasWebpage { Button("Open app", systemImage: "arrow.up.right.square") { Task { await model.openApp(app) } }.disabled(app.runningState != .running) }
                    AppControlMenu(model: model, app: appName, state: app.runningState)
                }
            }
            if let data = snapshot ?? initialSnapshot {
                let services = data.rows.filter { $0.isService && $0.app == appName }
                if !services.isEmpty {
                    Section("Services") {
                        ForEach(services) { service in
                            VStack(alignment: .leading, spacing: 4) {
                                Text(service.name)
                                Text("CPU " + percent(service.cpu) + " · " + bytes(service.memory)).font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                }
            }
        }.navigationTitle("App details").navigationBarTitleDisplayMode(.inline)
            .toolbar {
                Menu {
                    Button("Logs", systemImage: "doc.text") { model.logApp = appName; showingLogs = true }
                    AppDeleteMenu(model: model, app: appName)
                } label: { Image(systemName: "ellipsis") }.accessibilityLabel("App actions")
            }
            .navigationDestination(isPresented: $showingLogs) { DeviceLogsPage(model: model) }
            .onAppear { visible = true }.onDisappear { visible = false }
            .task(id: visible) {
                guard visible else { return }
                while !Task.isCancelled {
                    await refresh()
                    do { try await Task.sleep(for: .seconds(2)) } catch { return }
                }
            }
            .refreshable { await model.refreshDetails(); await refresh() }
    }
    private func refresh() async {
        do { let next = try await model.resources(); if !Task.isCancelled { snapshot = next; error = nil } }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        do { let next = try await model.ports(app: appName); if !Task.isCancelled { ports = next; portError = nil } }
        catch { if !Task.isCancelled { portError = error.localizedDescription } }
        loadingPorts = false
    }
}
#endif
