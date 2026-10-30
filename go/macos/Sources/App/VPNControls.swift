import SwiftUI

struct VPNToolbarControls: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    @ObservedObject var vpn: VPNController
    @State private var showingDevices = false

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        HStack(spacing: 10) {
            Text("VPN").font(.callout)
            Toggle("VPN", isOn: Binding(get: { vpn.enabled }, set: { enabled in
                showingDevices = true
                actions.submit { if enabled { await model.enableVPN() } else { await model.disableVPN() } }
            })).labelsHidden().accessibilityLabel("VPN").toggleStyle(.switch).controlSize(.small)
                .disabled(model.vpnUpdating || vpn.changing)
                .help(vpn.setupIssue ?? vpn.status)
            if model.vpnUpdating || vpn.changing { ProgressView().controlSize(.small) }
            Button { showingDevices.toggle() } label: {
                Image(systemName: "chevron.down")
                    .frame(width: 24, height: 28).contentShape(Rectangle())
            }.buttonStyle(.plain).help("VPN devices and status")
                .accessibilityLabel("VPN devices and status")
        }.padding(.leading, 10).padding(.trailing, 4).padding(.vertical, 4)
            .fixedSize()
            .popover(isPresented: $showingDevices) { VPNPopover(model: model, vpn: vpn) }
    }
}

struct VPNPopover: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    @ObservedObject var vpn: VPNController
    var showsWindowActions = false
    @Environment(\.openWindow) private var openWindow

    private var devices: [Device] {
        model.nearby.filter { $0.transport == .mesh }.sorted { $0.title.localizedStandardCompare($1.title) == .orderedAscending }
    }
    private var deviceListHeight: CGFloat {
        devices.isEmpty ? 72 : min(320, CGFloat(devices.count) * 64 - 6)
    }
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Wendy VPN").font(.headline)
                    Text(vpn.status).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Toggle("VPN", isOn: Binding(get: { vpn.enabled }, set: { enabled in
                    actions.submit { if enabled { await model.enableVPN() } else { await model.disableVPN() } }
                })).labelsHidden().accessibilityLabel("VPN").toggleStyle(.switch)
                    .disabled(model.vpnUpdating || vpn.changing)
            }
            if let issue = vpn.setupIssue {
                Label(issue, systemImage: "info.circle")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    .padding(12).frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 10))
            }
            if let error = model.vpnError ?? vpn.connectionError, error != vpn.setupIssue {
                FailureNote(message: error)
            }
            Divider()
            Label("All devices over mesh", systemImage: "network")
                .font(.subheadline.weight(.semibold))
            Text("Devices join automatically. If your normal internet goes down, Wendy uses an available mesh uplink and switches back when it recovers.")
                .font(.callout).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            ScrollView {
                LazyVStack(spacing: 6) {
                    ForEach(devices) { device in
                            HStack(spacing: 12) {
                                Image(systemName: "network")
                                    .foregroundStyle(.secondary).frame(width: 20)
                                VStack(alignment: .leading, spacing: 3) {
                                    Text(device.title).font(.callout.weight(.medium)).lineLimit(1)
                                    Text(device.transport.rawValue).font(.caption).foregroundStyle(.secondary)
                                }
                                Spacer(minLength: 8)
                            }.padding(.horizontal, 12).frame(height: 58).frame(maxWidth: .infinity, alignment: .leading)
                                .background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 10))
                                .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(Color.primary.opacity(0.08)))
                                .contentShape(RoundedRectangle(cornerRadius: 10))
                            .help(device.title)
                            .accessibilityLabel(device.title)

                    }
                }
                if devices.isEmpty { Text("Mesh devices appear here automatically.").foregroundStyle(.secondary).font(.callout).padding(.vertical) }
            }.frame(height: deviceListHeight)
            if showsWindowActions {
                Divider()
                HStack {
                    Button("Open Wendy") { openWindow(id: "main"); NSApp.activate(ignoringOtherApps: true) }
                    SettingsLink { Image(systemName: "gearshape") }.help("Settings")
                    Spacer()
                    Button("Quit") { NSApp.terminate(nil) }
                }
            }
        }.padding(20).frame(width: 420).fixedSize(horizontal: false, vertical: true)

    }

}
