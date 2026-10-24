import SwiftUI

@MainActor private final class ModelLibrary: ObservableObject {
    let session: UInt64
    @Published var query = ""
    @Published var repositories: [HubRepository] = []
    @Published var selection: String?
    @Published var details: [String: HubModelDetails] = [:]
    @Published var hardware: ModelHardware?
    @Published var file = ""
    @Published var port: String
    var accelerator: UInt32 { hardware?.suggestedAccelerator ?? 0 }
    @Published var install = true
    @Published var acceptWarning = false
    @Published var searching = false
    @Published var inspecting = false
    @Published var running = false
    @Published var progress = ""
    @Published var error: String?
    private var job: UInt64 = 0
    private var runTask: Task<Void, Never>?
    init(session: UInt64, port: String) { self.session = session; self.port = port }
    var selectedDetails: HubModelDetails? { selection.flatMap { details[$0] } }
    var selectedFile: HubModelFile? { selectedDetails?.files.first { $0.name == file } }
    var fit: ModelFit { selectedFile.flatMap { hardware?.fit($0, accelerator: accelerator, install: install) } ?? .unknown }
    var portNumber: UInt32? { guard let p = UInt32(port), (1...65535).contains(p) else { return nil }; return p }
    var canRun: Bool { !running && !inspecting && selectedFile?.unavailableReason.isEmpty == true && (install || portNumber != nil) && (!install || hardware?.canInstall == true) && (fit == .comfortable || acceptWarning) }
    var sortedRepositories: [HubRepository] {
        guard query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return repositories }
        return repositories.sorted {
            let a = details[$0.id]?.files.first.flatMap { hardware?.fit($0, accelerator: accelerator, install: install) } == .comfortable
            let b = details[$1.id]?.files.first.flatMap { hardware?.fit($0, accelerator: accelerator, install: install) } == .comfortable
            if a != b { return a }
            let sa = details[$0.id]?.files.first?.bytes ?? Int64.max
            let sb = details[$1.id]?.files.first?.bytes ?? Int64.max
            return sa == sb ? $0.id < $1.id : sa < sb
        }
    }
    func refreshHardware() async {
        do {
            let result = try await GoClient.shared.modelHardware(session: session)
            guard !Task.isCancelled else { return }
            hardware = result
            if !result.canInstall { install = false }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    func search() async {
        let text = query
        searching = true; error = nil
        defer { if !Task.isCancelled { searching = false } }
        do {
            if !text.isEmpty { try await Task.sleep(for: .milliseconds(350)) }
            let rows = try await GoClient.shared.searchHubModels(text)
            guard !Task.isCancelled, query == text else { return }
            repositories = rows
            if !rows.contains(where: { $0.id == selection }) { selection = rows.first?.id }
            if text.isEmpty {
                // Fetch actual file sizes before marking a starter model as a
                // suitable choice. Never infer capacity from parameter count.
                await withTaskGroup(of: (String, HubModelDetails?).self) { group in
                    for repo in rows where details[repo.id] == nil {
                        group.addTask { (repo.id, try? await GoClient.shared.hubModelDetails(repo.id)) }
                    }
                    for await (id, value) in group { if !Task.isCancelled, let value { details[id] = value } }
                }
            }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    func inspect() async {
        guard let id = selection else { return }
        file = ""; acceptWarning = false; inspecting = true; error = nil
        defer { if !Task.isCancelled { inspecting = false } }
        do {
            let value: HubModelDetails
            if let cached = details[id] { value = cached }
            else { value = try await GoClient.shared.hubModelDetails(id) }
            guard !Task.isCancelled, selection == id else { return }
            details[id] = value; file = value.files.first?.name ?? ""
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    func run(ready: @escaping (String, UInt32) -> Void) {
        guard canRun, let repo = selection, let detail = selectedDetails else { return }
        let port: UInt32 = install ? 11434 : (portNumber ?? 11434)
        running = true; error = nil; progress = "Checking device and model"
        job = UInt64.random(in: 1...UInt64.max)
        let request = job
        runTask = Task {
            defer { running = false }
            do {
                try await GoClient.shared.runModel(session: session, job: request, port: port, accelerator: accelerator, repo: repo, file: file, revision: detail.revision, install: install, acceptWarning: acceptWarning)
                guard !Task.isCancelled else { try? await GoClient.shared.cancelModelRun(session: session, job: request); return }
                while !Task.isCancelled {
                    let result = try await GoClient.shared.modelRunStatus(session: session, job: request)
                    guard !Task.isCancelled else { return }
                    progress = result.progress
                    if result.done {
                        if result.error.isEmpty { ready(result.model, result.port) }
                        else { error = result.error; if result.error.contains("capacity") { await refreshHardware(); acceptWarning = false } }
                        return
                    }
                    try await Task.sleep(for: .milliseconds(300))
                }
            } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        }
    }
    func cancel() { let request = job; Task { try? await GoClient.shared.cancelModelRun(session: session, job: request) } }
    func disappear() { if running { cancel() }; runTask?.cancel() }
}

struct ModelLibraryView: View {
    let deviceName: String
    let ready: (String, UInt32) -> Void
    @Environment(\.dismiss) private var dismiss
    @StateObject private var library: ModelLibrary
    init(session: UInt64, deviceName: String, port: String, ready: @escaping (String, UInt32) -> Void) {
        self.deviceName = deviceName; self.ready = ready
        _library = StateObject(wrappedValue: ModelLibrary(session: session, port: port))
    }
    private func size(_ value: Int64) -> String { ByteCountFormatter.string(fromByteCount: value, countStyle: .memory) }
    private func fitColor(_ fit: ModelFit) -> Color { switch fit { case .comfortable: .green; case .tight: .orange; case .tooLarge: .red; case .unknown: .secondary } }
    var body: some View {
        VStack(spacing: 0) {
            HStack {
                VStack(alignment: .leading, spacing: 3) {
                    Text("Model library").font(.title2.bold())
                    Text("Download and run on \(deviceName)").foregroundStyle(.secondary)
                }
                Spacer()
                Button("Done") { dismiss() }.disabled(library.running).keyboardShortcut(.cancelAction)
            }.padding(20)
            Divider()
            HSplitView {
                VStack(spacing: 10) {
                    HStack {
                        Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                        TextField("Search Hugging Face", text: $library.query).textFieldStyle(.plain)
                        if !library.query.isEmpty { Button { library.query = "" } label: { Image(systemName: "xmark.circle.fill") }.buttonStyle(.plain) }
                    }.padding(9).background(.quaternary, in: RoundedRectangle(cornerRadius: 8)).padding(.horizontal, 12).padding(.top, 12)
                    HStack {
                        Text(library.query.isEmpty ? "Qwen & NVIDIA" : "Public GGUF models").font(.caption.weight(.medium)).foregroundStyle(.secondary)
                        Spacer()
                        if library.searching { ProgressView().controlSize(.small) }
                    }.padding(.horizontal, 16)
                    List(library.sortedRepositories, selection: $library.selection) { repo in
                        VStack(alignment: .leading, spacing: 5) {
                            Text(repo.name).font(.body.weight(.medium)).lineLimit(2)
                            if !repo.detail.isEmpty { Text(repo.detail).font(.caption).foregroundStyle(.secondary).lineLimit(3) }
                            if let file = library.details[repo.id]?.files.first {
                                let fit = library.hardware?.fit(file, accelerator: library.accelerator, install: library.install) ?? .unknown
                                Text("\(size(file.bytes)) · \(fit.title)").font(.caption2).foregroundStyle(fitColor(fit))
                            } else { Text(repo.id.components(separatedBy: "/").first ?? repo.id).font(.caption).foregroundStyle(.secondary) }
                        }.padding(.vertical, 7).tag(repo.id)
                    }.listStyle(.sidebar)
                }.frame(minWidth: 270, idealWidth: 300, maxWidth: 350).disabled(library.running)
                ScrollView {
                    VStack(alignment: .leading, spacing: 18) {
                        hardwareSummary
                        if library.inspecting { ProgressView("Reading model files") }
                        if let repo = library.selection, let detail = library.selectedDetails {
                            HStack(alignment: .top) {
                                VStack(alignment: .leading, spacing: 6) {
                                    Text(repo.components(separatedBy: "/").last ?? repo).font(.title3.bold()).textSelection(.enabled)
                                    Text(repo).font(.caption).foregroundStyle(.secondary)
                                }
                                Spacer()
                                Link(destination: URL(string: "https://huggingface.co/" + repo)!) { Image(systemName: "arrow.up.right.square") }.help("Model card and license")
                            }
                            Picker("Weights", selection: $library.file) {
                                ForEach(detail.files) { Text("\($0.quantization) · \(size($0.bytes))").tag($0.name) }
                            }.disabled(library.running)
                            if let file = library.selectedFile {
                                if !file.unavailableReason.isEmpty {
                                    Label(file.unavailableReason, systemImage: "info.circle").font(.callout).foregroundStyle(.secondary)
                                }
                                VStack(alignment: .leading, spacing: 8) {
                                    Label(library.fit.title, systemImage: library.fit == .comfortable ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                                        .font(.headline).foregroundStyle(fitColor(library.fit))
                                    Text("Estimated memory: \(size(file.estimatedMemory)) · Download: \(size(file.bytes))")
                                    Text("Estimate includes weights, 25% overhead and 4 GiB for the runtime and a 32,768-token context. Actual memory varies by model; this is not a benchmark.")
                                        .font(.caption).foregroundStyle(.secondary)
                                    if library.install { Text("Allow an additional 12 GiB of free storage for the runtime image and extraction.").font(.caption).foregroundStyle(.secondary) }
                                }.padding(12).frame(maxWidth: .infinity, alignment: .leading)
                                    .background(fitColor(library.fit).opacity(0.07), in: RoundedRectangle(cornerRadius: 10))
                            }
                            Text("License: \(detail.license.isEmpty ? "See model card" : detail.license)").font(.caption).foregroundStyle(.secondary)
                            if library.selectedFile?.unavailableReason.isEmpty == true {
                            Divider()
                            VStack(alignment: .leading, spacing: 12) {
                                Toggle("Install or start a Wendy-managed Ollama runtime", isOn: $library.install).disabled(library.hardware?.canInstall != true)
                                HStack {
                                    if library.install {
                                        Label("Automatic connection", systemImage: "network").foregroundStyle(.secondary)
                                    } else {
                                        Text("Port")
                                        TextField("11434", text: $library.port).textFieldStyle(.roundedBorder).frame(width: 76)
                                    }
                                    Spacer()
                                    if let hardware = library.hardware {
                                        Label(hardware.computeName, systemImage: hardware.cuda ? "sparkles" : "cpu")
                                            .foregroundStyle(.secondary)
                                            .help(library.install ? "Selected automatically from this device’s hardware" : "Capacity estimate uses this device’s supported hardware; the existing server controls execution")
                                    } else {
                                        ProgressView("Detecting compute").controlSize(.small)
                                    }
                                }
                                Text(library.install ? "Wendy reuses a matching runtime or selects an available port automatically. Models stay on the device." : "Use an existing Ollama server on this port. Its compute settings remain unchanged.")
                                    .font(.caption).foregroundStyle(.secondary)
                                if library.install {
                                    Text(library.hardware?.cuda == true ? "GPU acceleration is selected automatically." : "CPU is used when no supported GPU is detected.")
                                        .font(.caption).foregroundStyle(.secondary)
                                }
                                if library.fit != .comfortable {
                                    Toggle("Run despite the capacity warning", isOn: $library.acceptWarning)
                                    Text("The model may run out of memory or storage. A smaller model or quantization is recommended.").font(.caption).foregroundStyle(.secondary)
                                }
                            }.disabled(library.running)
                            }
                        }
                        if let error = library.error {
                            Label(error, systemImage: "exclamationmark.triangle").font(.callout).foregroundStyle(.orange).textSelection(.enabled)
                        }
                    }.padding(20)
                }.frame(minWidth: 470, maxWidth: .infinity)
            }
            Divider()
            HStack(spacing: 12) {
                if library.running { ProgressView().controlSize(.small); Text(library.progress).lineLimit(2); Spacer(); Button("Cancel download") { library.cancel() } }
                else {
                    if let file = library.selectedFile, !file.unavailableReason.isEmpty {
                        Label("Split GGUF download is not supported yet", systemImage: "info.circle")
                            .font(.callout).foregroundStyle(.secondary)
                        Spacer()
                        if let repo = library.selection, let url = URL(string: "https://huggingface.co/" + repo) {
                            Link("View model", destination: url)
                        }
                    } else {
                        Text("Public GGUF weights. Inference runs on your device.").font(.caption).foregroundStyle(.secondary)
                        Spacer()
                        Button("Download & Run") { library.run { model, port in ready(model, port); dismiss() } }
                            .buttonStyle(.borderedProminent).disabled(!library.canRun)
                    }
                }
            }.padding(16)
        }
        .frame(width: 900, height: 680)
        .interactiveDismissDisabled(library.running)
        .task { await library.refreshHardware() }
        .task(id: library.query) { await library.search() }
        .task(id: library.selection) { await library.inspect() }
        .onChange(of: library.file) { _, _ in library.acceptWarning = false }
        .onChange(of: library.accelerator) { _, _ in library.acceptWarning = false }
        .onChange(of: library.install) { _, _ in library.acceptWarning = false }
        .onDisappear { library.disappear() }
    }
    @ViewBuilder private var hardwareSummary: some View {
        if let h = library.hardware {
            VStack(alignment: .leading, spacing: 5) {
                Label(deviceName, systemImage: "desktopcomputer").font(.headline)
                Text(h.memoryTotal > 0 ? "\(size(h.memoryAvailable)) memory available of \(size(h.memoryTotal))" : "Memory capacity unknown")
                if !h.gpu.isEmpty { Text("\(h.gpu)\(h.sharedMemory ? " · shared memory" : h.gpuTotal > 0 ? " · \(size(h.gpuAvailable)) GPU memory available" : " · GPU memory unknown")") }
                Text(h.diskKnown ? "\(size(h.diskFree)) storage free" : "Storage capacity unknown")
                if !h.warning.isEmpty { Text(h.warning).foregroundStyle(.orange) }
                if !h.canInstall { Text("Automatic runtime installation requires Linux on arm64 or x86-64. An existing Ollama server can still be used.").foregroundStyle(.secondary) }
            }.font(.caption).frame(maxWidth: .infinity, alignment: .leading)
                .padding(12).background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 10))
        } else { ProgressView("Checking device capacity") }
    }
}
