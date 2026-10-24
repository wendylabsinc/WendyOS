import SwiftUI

struct DeviceAudioEndpoint: Identifiable {
    let id: UInt32
    let name, detail: String
    let direction: Int32
    let isDefault: Bool
    var title: String { detail.isEmpty ? name : detail }
}
extension GoClient {
    func audioDevices(session: UInt64) async throws -> [DeviceAudioEndpoint] {
        try await perform {
            guard let p = WDAudioDevices(session) else { throw WendyError(message: "Missing audio devices.") }
            defer { WDAudioFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.devices, count: p.pointee.count).map {
                DeviceAudioEndpoint(id: $0.id, name: Self.string($0.name), detail: Self.string($0.description), direction: $0.direction, isDefault: $0.is_default != 0)
            }
        }
    }
    func setDefaultAudio(session: UInt64, device: DeviceAudioEndpoint) async throws {
        try await perform { try Self.finish(WDSetDefaultAudio(session, device.id, device.direction)) }
    }
}
struct DeviceAudioView: View {
    let session: UInt64
    let deviceName: String
    @State private var devices: [DeviceAudioEndpoint] = []
    @State private var loading = true
    @State private var saving: UInt32?
    @State private var error: String?
    var body: some View {
        Form {
            if let error { Section { FailureNote(message: error) } }
            if loading && devices.isEmpty {
                Section { HStack { ProgressView(); Text("Loading audio devices…").foregroundStyle(.secondary) } }
            } else {
                endpoints("Default microphone", direction: 1, symbol: "mic")
                endpoints("Default speaker", direction: 2, symbol: "speaker.wave.2")
                Section { Text("These defaults apply to \(deviceName). Apps with an explicit audio device keep their own selection.").font(.footnote).foregroundStyle(.secondary) }
            }
        }.formStyle(.grouped).navigationTitle("Audio")
            .task(id: session) { await refresh() }
            .mobileRefreshable { if saving == nil { await refresh() } }
            #if os(macOS)
            .toolbar { Button { Task { await refresh() } } label: { Image(systemName: "arrow.clockwise") }.accessibilityLabel("Refresh audio devices").disabled(loading || saving != nil) }
            #endif
    }
    private func endpoints(_ title: String, direction: Int32, symbol: String) -> some View {
        Section(title) {
            let choices = devices.filter { $0.direction == direction }
            if choices.isEmpty { Text(direction == 1 ? "No microphones found" : "No speakers found").foregroundStyle(.secondary) }
            ForEach(choices) { device in
                Button { Task { await select(device) } } label: {
                    HStack(spacing: 12) {
                        Image(systemName: symbol).foregroundStyle(.secondary).frame(width: 24)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(device.title).foregroundStyle(.primary).multilineTextAlignment(.leading)
                            if !device.name.isEmpty && device.name != device.title { Text(device.name).font(.caption).foregroundStyle(.secondary).lineLimit(2) }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                        if saving == device.id { ProgressView() }
                        else if device.isDefault { Image(systemName: "checkmark").foregroundStyle(Color.accentColor).accessibilityLabel("Default") }
                    }.padding(.vertical, 4).contentShape(Rectangle())
                }.buttonStyle(.plain).disabled(loading || saving != nil)
                    .accessibilityValue(device.isDefault ? "Default" : "Not selected")
            }
        }
    }
    private func refresh() async {
        loading = true; error = nil
        defer { loading = false }
        do { let next = try await GoClient.shared.audioDevices(session: session); if !Task.isCancelled { devices = next } }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func select(_ device: DeviceAudioEndpoint) async {
        guard !device.isDefault && saving == nil else { return }
        saving = device.id; error = nil
        defer { saving = nil }
        do {
            try await GoClient.shared.setDefaultAudio(session: session, device: device)
            await refresh()
            if error == nil && !devices.contains(where: { $0.id == device.id && $0.isDefault }) {
                error = "The change was accepted, but the device has not reported this default yet. Pull to refresh to check again."
            }
        } catch { self.error = error.localizedDescription }
    }
}
