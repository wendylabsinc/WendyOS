import SwiftUI

struct PaneRefreshAction {
    let enabled: Bool
    let perform: () -> Void
}
private struct OrganizationRefreshKey: FocusedValueKey {
    typealias Value = PaneRefreshAction
}
extension FocusedValues {
    var organizationRefresh: PaneRefreshAction? {
        get { self[OrganizationRefreshKey.self] }
        set { self[OrganizationRefreshKey.self] = newValue }
    }
}

private struct DevicePaneRefreshKey: FocusedValueKey { typealias Value = PaneRefreshAction }
extension FocusedValues {
    var devicePaneRefresh: PaneRefreshAction? {
        get { self[DevicePaneRefreshKey.self] }
        set { self[DevicePaneRefreshKey.self] = newValue }
    }
}

private struct FleetRefreshKey: FocusedValueKey { typealias Value = PaneRefreshAction }
extension FocusedValues {
    var fleetRefresh: PaneRefreshAction? {
        get { self[FleetRefreshKey.self] }
        set { self[FleetRefreshKey.self] = newValue }
    }
}

#if os(macOS)
/// Keep focused-action observation outside the view that creates the panes.
/// Their closures change when they render; observing those values in the parent
/// would recreate the publishers again during toolbar/focus reconciliation.
struct DesktopPaneRefreshButton: View {
    @ObservedObject var model: AppModel
    let actions: AsyncActions
    let showingSimulators: Bool
    let showingSites: Bool
    let showingOrganization: Bool
    let tab: String
    let deviceSource: String
    @State private var refreshingPane = false
    @FocusedValue(\.simulatorRefresh) private var simulatorRefresh
    @FocusedValue(\.organizationRefresh) private var organizationRefresh
    @FocusedValue(\.devicePaneRefresh) private var devicePaneRefresh
    @FocusedValue(\.fleetRefresh) private var fleetRefresh

    var body: some View {
        Button {
            if showingSimulators { simulatorRefresh?.perform() }
            else if showingSites { fleetRefresh?.perform() }
            else if showingOrganization { organizationRefresh?.perform() }
            else if ["campaigns", "pairing", "chat", "data", "settings"].contains(tab), let devicePaneRefresh { devicePaneRefresh.perform() }
            else {
                refreshingPane = true
                actions.submit {
                    defer { refreshingPane = false }
                    if model.connected != nil { await model.refreshDetails() }
                    else if deviceSource == "cloud" { await model.refreshCloud() }
                    else { await model.refreshNearby() }
                }
            }
        } label: { Label("Refresh", systemImage: "arrow.clockwise") }
            .help(showingSimulators ? "Refresh simulators" : showingSites ? "Refresh fleet" : showingOrganization ? "Refresh organization" : model.connected != nil ? "Refresh device" : "Refresh devices")
            .keyboardShortcut("r", modifiers: .command)
            .disabled(!showingSimulators && !showingSites && !showingOrganization && model.selection != nil && !model.selectedDeviceReady)
            .disabled(showingSimulators ? simulatorRefresh?.enabled != true : showingSites ? fleetRefresh?.enabled != true : showingOrganization ? organizationRefresh?.enabled != true : ["campaigns", "pairing", "chat", "data", "settings"].contains(tab) ? devicePaneRefresh?.enabled != true : refreshingPane || model.discovering || !model.initialized)
    }
}
#endif
