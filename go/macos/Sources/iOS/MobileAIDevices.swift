import SwiftUI
import CryptoKit

struct RememberedAIModel: Codable, Equatable, Identifiable {
    let id: String
    let name: String
    let port: UInt32
}
struct RememberedAIDevice: Codable, Equatable, Identifiable {
    let id: String
    var name: String
    var models: [RememberedAIModel]
    var checked: Date
    var lastUsed: Date?
    var lastModel: String?
    var preferred: RememberedAIModel? { models.first { $0.id == lastModel } ?? models.first }
}

// Store only verified discovery metadata, never credentials, prompts or endpoints.
@MainActor final class RememberedAIDevices: ObservableObject {
    @Published private(set) var revision = 0
    private let defaults: UserDefaults
    init(defaults: UserDefaults = .standard) { self.defaults = defaults }
    private func key(account: String, organization: String) -> String {
        let data = try! JSONEncoder().encode([account, organization])
        return "wendy.aiDevices.v1." + SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
    func records(account: String, organization: String) -> [RememberedAIDevice] {
        guard let data = defaults.data(forKey: key(account: account, organization: organization)) else { return [] }
        return (try? JSONDecoder().decode([RememberedAIDevice].self, from: data)) ?? []
    }
    func discovered(device: Device, models: [ChatModelOption], account: String, organization: String, now: Date = Date()) {
        var rows = records(account: account, organization: organization)
        let id = recordingDeviceID(device)
        let previous = rows.first { $0.id == id }
        rows.removeAll { $0.id == id }
        let usable = models.filter { $0.unavailableReason.isEmpty }.prefix(40).map { RememberedAIModel(id: $0.id, name: $0.displayName, port: $0.port) }
        if !usable.isEmpty {
            rows.append(RememberedAIDevice(id: id, name: device.title, models: usable, checked: now,
                                          lastUsed: previous?.lastUsed, lastModel: previous?.lastModel))
        }
        persist(rows, account: account, organization: organization)
    }
    func used(deviceID: String, model: String, account: String, organization: String, now: Date = Date()) {
        var rows = records(account: account, organization: organization)
        guard let i = rows.firstIndex(where: { $0.id == deviceID }), rows[i].models.contains(where: { $0.id == model }) else { return }
        rows[i].lastUsed = now; rows[i].lastModel = model
        persist(rows, account: account, organization: organization)
    }
    private func persist(_ rows: [RememberedAIDevice], account: String, organization: String) {
        if let data = try? JSONEncoder().encode(Array(rows.sorted { ($0.lastUsed ?? $0.checked) > ($1.lastUsed ?? $1.checked) }.prefix(50))) {
            defaults.set(data, forKey: key(account: account, organization: organization)); revision += 1
        }
    }
}

struct MobileAIDeviceSuggestions: View {
    @ObservedObject var model: AppModel
    @ObservedObject var remembered: RememberedAIDevices
    let chooseDevice: () -> Void
    let select: (RememberedAIDevice, Device) -> Void
    var excludedID: String?
    private var suggestions: [(RememberedAIDevice, Device, String)] {
        let devices = HomeWidgetInventory.devices(model)
        return remembered.records(account: model.mobileCloudAccount, organization: model.mobileCloudOrganization).compactMap { record in
            guard record.id != excludedID, let device = devices.first(where: { recordingDeviceID($0) == record.id }) else { return nil }
            return (record, device, HomeWidgetInventory.status(device, model: model))
        }.sorted {
            let available: (String) -> Bool = { ["Online", "Nearby", "Connected"].contains($0) }
            if available($0.2) != available($1.2) { return available($0.2) }
            return ($0.0.lastUsed ?? .distantPast) > ($1.0.lastUsed ?? .distantPast)
        }
    }
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text("Choose an AI device").font(.title2.bold())
                if suggestions.isEmpty {
                    Text("Devices with models appear here after we’ve checked them.").foregroundStyle(.secondary)
                } else {
                    Text(suggestions.allSatisfy { $0.2 == "Offline" }
                         ? "These devices are offline. Choose another device or try again when they’re online."
                         : "Previously found models. We’ll check again when you connect.").font(.subheadline).foregroundStyle(.secondary)
                    VStack(spacing: 0) {
                        ForEach(suggestions, id: \.0.id) { record, device, status in
                            Button { select(record, device) } label: {
                                HStack(spacing: 12) {
                                    MobileDeviceAvatar(device: device, size: 42).saturation(status == "Offline" ? 0 : 1)
                                    VStack(alignment: .leading, spacing: 3) {
                                        Text(device.title).font(.headline).foregroundStyle(.primary).lineLimit(1)
                                        Text(record.preferred?.name ?? "Model").font(.subheadline).foregroundStyle(.secondary).lineLimit(1)
                                        Text(status + " · Checked " + record.checked.formatted(.relative(presentation: .named)))
                                            .font(.caption).foregroundStyle(.secondary)
                                    }.frame(maxWidth: .infinity, alignment: .leading)
                                    Image(systemName: "chevron.right").font(.caption).foregroundStyle(.secondary)
                                }.padding(16).contentShape(.rect)
                            }.buttonStyle(.plain).disabled(status == "Offline" || !model.mobileScopeIsCurrent)
                            if record.id != suggestions.last?.0.id { Divider().padding(.leading, 70) }
                        }
                    }.background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
                }
                Button("Choose another device", action: chooseDevice).mobileActionStyle()
                Text("For a cloud model, choose a device for context, then select Cloud.").font(.caption).foregroundStyle(.secondary)
            }.padding(20)
        }.background(MobileLensStyle.background)
    }
}
