import SwiftUI

struct MobileVPNView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var vpn: VPNController
    @State private var actions = AsyncActions()
    private var devices: [Device] { model.nearby.filter { $0.transport == .mesh } }

    var body: some View {
        Form {
            Section {
                Toggle("VPN", isOn: Binding(get: { vpn.enabled }, set: { enabled in
                    actions.submit { if enabled { await model.enableVPN() } else { await model.disableVPN() } }
                })).disabled(model.vpnUpdating || vpn.changing)
                HStack {
                    Text(vpn.status).foregroundStyle(.secondary)
                    if model.vpnUpdating || vpn.changing { ProgressView() }
                }
                if let issue = vpn.setupIssue { Text(issue) }
                if let error = model.vpnError ?? vpn.connectionError { FailureNote(message: error) }
            }
            Section("Automatic internet backup") {
                Text("If your normal internet goes down, Wendy uses an available mesh uplink and switches back when it recovers.")
            }
            Section("All devices over mesh") {
                ForEach(devices) { device in
                    Label(device.title, systemImage: "network")
                }
                if devices.isEmpty { Text("Mesh devices appear here automatically.").foregroundStyle(.secondary) }
            }
        }.navigationTitle("Wendy VPN").task { await actions.run() }
    }
}
