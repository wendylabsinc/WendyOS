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
        (model.nearby + model.cloud).filter(\.routable).sorted { $0.title.localizedStandardCompare($1.title) == .orderedAscending }
    }
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Wendy VPN").font(.headline)
                    Text(model.vpnUpdating ? "Updating…" : vpn.status).font(.caption).foregroundStyle(.secondary)
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
            if let error = model.vpnError, error != vpn.setupIssue {
                FailureNote(message: error)
            }
            if !vpn.enabled && model.selectedRoutes.isEmpty {
                Text("Select at least one device, then turn on VPN.")
                    .font(.callout).foregroundStyle(.secondary)
            }
            Divider()
            HStack {
                Text("Devices").font(.subheadline.weight(.semibold))
                Spacer()
                Text("\(model.selectedRoutes.count) selected").font(.caption).foregroundStyle(.secondary)
            }
            ScrollView {
                LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 10) {
                    ForEach(devices) { device in
                        let selected = model.selectedRoutes.contains(device.id)
                        Button {
                            if selected { model.selectedRoutes.remove(device.id) }
                            else { model.selectedRoutes.insert(device.id) }
                        } label: {
                            VStack(alignment: .leading, spacing: 8) {
                                HStack {
                                    Image(systemName: device.transport == .cloud ? "cloud" : "network").foregroundStyle(.secondary)
                                    Spacer()
                                    Image(systemName: selected ? "checkmark.circle.fill" : "circle")
                                        .foregroundStyle(selected ? Color.accentColor : .secondary)
                                }
                                Text(device.title).font(.callout.weight(.medium)).lineLimit(2).frame(height: 34, alignment: .topLeading)
                                Text(device.transport.rawValue).font(.caption).foregroundStyle(.secondary)
                            }.padding(12).frame(maxWidth: .infinity, alignment: .leading)
                                .background(selected ? Color.accentColor.opacity(0.1) : Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 10))
                                .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(selected ? Color.accentColor.opacity(0.5) : Color.primary.opacity(0.08)))
                                .contentShape(RoundedRectangle(cornerRadius: 10))
                        }.buttonStyle(.plain).accessibilityLabel(device.title).accessibilityValue(selected ? "Selected" : "Not selected")
                    }
                }
                if devices.isEmpty { Text("Sign in and refresh your devices to add a route.").foregroundStyle(.secondary).font(.callout).padding(.vertical) }
            }.frame(maxHeight: 330)
            if vpn.active {
                Button("Apply Selection") { actions.submit { await model.enableVPN() } }
                    .disabled(model.vpnUpdating || model.selectedRoutes.isEmpty)
            }
            Text("Routes selected devices over mesh or cloud. Other internet traffic uses your normal connection.")
                .font(.caption).foregroundStyle(.secondary)
            if !vpn.routes.isEmpty {
                DisclosureGroup("Device addresses") {
                    ForEach(vpn.routes) { route in
                        VStack(alignment: .leading, spacing: 4) {
                            Text(route.name).font(.caption.weight(.medium))
                            Text(route.host).font(.caption.monospaced()).textSelection(.enabled)
                            Text(route.address).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(.vertical, 4)
                    }
                }
            }
            if showsWindowActions {
                Divider()
                HStack {
                    Button("Open Wendy") { openWindow(id: "main"); NSApp.activate(ignoringOtherApps: true) }
                    SettingsLink { Image(systemName: "gearshape") }.help("Settings")
                    Spacer()
                    Button("Quit") { NSApp.terminate(nil) }
                }
            }
        }.padding(20).frame(width: 420)
    }
}
