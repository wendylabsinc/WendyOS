import SwiftUI

private struct DeviceSiteModel: Decodable, Identifiable, Sendable {
    let id: String
    let name: String?
    let size: Int64?
}
private extension GoClient {
    // A transfer can take minutes, and the shared request queue runs one call at a time.
    static let siteModelQueue = DispatchQueue(label: "dev.wendy.site-models", qos: .userInitiated)

    func siteModels(session: UInt64, action: String, path: String = "", name: String = "", id: String = "") async throws -> [DeviceSiteModel] {
        try await BlockingWork.run(on: GoClient.siteModelQueue) {
            try Self.withCString(action) { action in
                try Self.withCString(path) { path in
                    try Self.withCString(name) { name in
                        try Self.withCString(id) { id in
                            guard let result = WDSiteModels(session, action, path, name, id) else { throw WendyError(message: "No model transfer result.") }
                            defer { WDSiteModelsFree(result) }
                            try Self.check(result.pointee.status)
                            return try JSONDecoder().decode([DeviceSiteModel]?.self, from: Data(Self.string(result.pointee.json).utf8)) ?? []
                        }
                    }
                }
            }
        }
    }
}

struct MobileSiteDeviceModels: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: MobileSitesStore
    let siteID: UUID
    /// A link means upload; nil means browse/download.
    let saving: MobileSiteModelLink?
    @Environment(\.dismiss) private var dismiss
    @State private var device: Device?
    @State private var remote: [DeviceSiteModel] = []
    @State private var session: UInt64?
    @State private var busy = false
    @State private var message: String?
    @State private var error: String?
    @State private var search = ""
    @State private var actions = AsyncActions()

    private var devices: [Device] {
        HomeWidgetInventory.devices(model).filter { search.isEmpty || $0.title.localizedCaseInsensitiveContains(search) }
            .sorted {
                let a = online($0), b = online($1)
                return a != b ? a : $0.title.localizedStandardCompare($1.title) == .orderedAscending
            }
    }
    private func online(_ device: Device) -> Bool {
        ["Online", "Connected", "Nearby"].contains(HomeWidgetInventory.status(device, model: model))
    }
    var body: some View {
        NavigationStack {
            List {
                if let device {
                    Section("Device") {
                        LabeledContent(device.title, value: session == nil ? "Connecting" : "Connected")
                        Button("Change device") { self.device = nil; session = nil; remote = []; error = nil }.disabled(busy)
                    }
                    if saving != nil {
                        Section {
                            Button("Save model to device", systemImage: "square.and.arrow.up") { transfer(nil) }
                                .disabled(busy || session == nil || message != nil)
                        } footer: { Text("Saves the 3D model, placements and scan references. The copy on your iPhone is kept.") }
                    } else if session != nil {
                        Section("Saved models") {
                            ForEach(remote) { item in
                                Button { transfer(item) } label: {
                                    VStack(alignment: .leading) {
                                        Text(item.name ?? "3D model")
                                        Text(ByteCountFormatter.string(fromByteCount: item.size ?? 0, countStyle: .file))
                                            .font(.caption).foregroundStyle(.secondary)
                                    }
                                }.disabled(busy)
                            }
                            if remote.isEmpty && !busy && error == nil { Text("No models saved on this device").foregroundStyle(.secondary) }
                            Button("Refresh") { connect(device) }.disabled(busy)
                        }
                    }
                } else {
                    Section("Choose a device") {
                        ForEach(devices) { item in
                            Button { connect(item) } label: {
                                HStack {
                                    Circle().fill(online(item) ? MobileLensStyle.green : Color.gray).frame(width: 8, height: 8)
                                    VStack(alignment: .leading) {
                                        Text(item.title)
                                        Text(HomeWidgetInventory.status(item, model: model)).font(.caption).foregroundStyle(.secondary)
                                    }
                                }
                            }.disabled(!online(item) || busy)
                        }
                    }
                }
                if busy { ProgressView(saving == nil ? "Loading models…" : "Preparing or saving model…") }
                if let message { Label(message, systemImage: "checkmark.circle.fill").foregroundStyle(MobileLensStyle.green) }
                if let error { Text(error).foregroundStyle(.red) }
            }
            .searchable(text: $search, prompt: "Search devices")
            .navigationTitle(saving == nil ? "Load from device" : "Save to device")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() }.disabled(busy) } }
        }.tint(MobileLensStyle.green).interactiveDismissDisabled(busy)
            .task { await actions.run() }
    }
    private func connect(_ target: Device) {
        device = target; busy = true; error = nil; message = nil; session = nil; remote = []
        let scope = model.mobileCloudSelectionKey
        actions.submit {
            defer { busy = false }
            await model.connect(target)
            guard !Task.isCancelled, scope == model.mobileCloudSelectionKey else { return }
            guard model.selectedDeviceReady, model.connected?.id == target.id else { error = model.connectionError?.message ?? "Could not connect to this device."; return }
            let captured = model.deviceSession
            do {
                // Probe support before offering upload, too.
                let values = try await GoClient.shared.siteModels(session: captured, action: "list")
                guard !Task.isCancelled, model.deviceSession == captured, scope == model.mobileCloudSelectionKey else { return }
                remote = values; session = captured
            } catch { self.error = error.localizedDescription }
        }
    }
    private func transfer(_ item: DeviceSiteModel?) {
        guard let session, model.deviceSession == session, let device else { error = "Reconnect to the device and try again."; return }
        let scope = model.mobileCloudSelectionKey
        busy = true; error = nil
        actions.submit {
            defer { busy = false }
            do {
                if let saving {
                    let url = try await store.models.shareArchive(id: saving.id)
                    try Task.checkCancellation()
                    _ = try await GoClient.shared.siteModels(session: session, action: "save", path: url.path, name: saving.name)
                    guard !Task.isCancelled, scope == model.mobileCloudSelectionKey else { return }
                    message = "Saved to \(device.title)"
                } else if let item {
                    let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".wendysite")
                    defer { try? FileManager.default.removeItem(at: url) }
                    _ = try await GoClient.shared.siteModels(session: session, action: "load", path: url.path, id: item.id)
                    try Task.checkCancellation()
                    guard scope == model.mobileCloudSelectionKey else { return }
                    try await store.addModel(from: url, to: siteID)
                    dismiss()
                }
            } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        }
    }
}
