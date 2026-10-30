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
