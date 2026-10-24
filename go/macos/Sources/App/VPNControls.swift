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
    private let selectionHint = "Select at least one device, then turn on VPN."
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
            if !vpn.enabled && model.selectedRoutes.isEmpty && model.vpnError != selectionHint && vpn.setupIssue != selectionHint {
                Text(selectionHint)
                    .font(.callout).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Divider()
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Devices").font(.subheadline.weight(.semibold))
                    Text("\(model.selectedRoutes.count) selected").font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                HStack(spacing: 12) {
                    Button("Select all") { model.selectedRoutes = Set(devices.map(\.id)) }
                        .disabled(devices.isEmpty || Set(devices.map(\.id)).isSubset(of: model.selectedRoutes))
                    Button("Select none") { model.selectedRoutes.removeAll() }
                        .disabled(model.selectedRoutes.isEmpty)
                }.buttonStyle(.link).font(.callout)
            }
            ScrollView {
                LazyVStack(spacing: 6) {
                    ForEach(devices) { device in
                        let selected = model.selectedRoutes.contains(device.id)
                        let route = vpn.routes.first { $0.selector == device.selector }
                        Button {
                            if selected { model.selectedRoutes.remove(device.id) }
                            else { model.selectedRoutes.insert(device.id) }
                        } label: {
                            HStack(spacing: 12) {
                                Image(systemName: device.transport == .cloud ? "cloud" : "network")
                                    .foregroundStyle(.secondary).frame(width: 20)
                                VStack(alignment: .leading, spacing: 3) {
                                    Text(device.title).font(.callout.weight(.medium)).lineLimit(1)
                                    Text(device.transport.rawValue).font(.caption).foregroundStyle(.secondary)
                                }
                                Spacer(minLength: 8)
                                Image(systemName: selected ? "checkmark.circle.fill" : "circle")
                                    .foregroundStyle(selected ? Color.accentColor : .secondary)
                            }.padding(.horizontal, 12).frame(height: 58).frame(maxWidth: .infinity, alignment: .leading)
                                .background(selected ? Color.accentColor.opacity(0.1) : Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 10))
                                .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(selected ? Color.accentColor.opacity(0.5) : Color.primary.opacity(0.08)))
                                .contentShape(RoundedRectangle(cornerRadius: 10))
                        }.buttonStyle(.plain).help(device.title)
                            .accessibilityLabel(device.title).accessibilityValue(selected ? "Selected" : "Not selected")
                            .contextMenu {
                                Button("Copy address", systemImage: "doc.on.doc") { copy(route?.address) }
                                    .disabled(route?.address.isEmpty != false)
                                Button("Copy hostname", systemImage: "doc.on.doc") { copy(route?.host) }
                                    .disabled(route?.host.isEmpty != false)
                            }
                    }
                }
                if devices.isEmpty { Text("Sign in and refresh your devices to add a route.").foregroundStyle(.secondary).font(.callout).padding(.vertical) }
            }.frame(height: deviceListHeight)
            if vpn.active {
                Button("Apply Selection") { actions.submit { await model.enableVPN() } }
                    .disabled(model.vpnUpdating || model.selectedRoutes.isEmpty)
            }
            Text("Routes selected devices over mesh or cloud. Other internet traffic uses your normal connection.")
                .font(.caption).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
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
            .onChange(of: model.selectedRoutes) { _, selection in
                if !selection.isEmpty && model.vpnError == selectionHint { model.vpnError = nil }
            }
    }

    private func copy(_ value: String?) {
        guard let value, !value.isEmpty else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(value, forType: .string)
    }
}
