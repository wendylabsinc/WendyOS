import SwiftUI

// This entry point is compiled only by the generated UI regression project.
// It has a separate bundle ID and does not instantiate WendyDesktopApp or its
// delegate, menu-bar tasks, account observation, VPN or discovery lifecycle.
@main
struct MenuFixtureApp: App {
    var body: some Scene {
        WindowGroup("Menu regression fixture") {
            MenuFixtureView()
        }
        .windowResizability(.contentSize)
    }
}

@MainActor
private struct MenuFixtureView: View {
    @StateObject private var apps: AppModel
    @StateObject private var simulators: SimulatorModel
    private let screen: String
    private let narrow: Bool

    init() {
        let arguments = ProcessInfo.processInfo.arguments
        screen = arguments.contains("--simulators") ? "simulators" : "apps"
        narrow = arguments.contains("--narrow")
        let name = arguments.contains("--long-name")
            ? "regression-fixture-with-a-long-name-that-constrains-the-action-column"
            : "regression-fixture"
        let busy = arguments.contains("--busy")
        let model = AppModel(loadDeviceApps: { [] }, loadDeviceCameras: { [] }, connectDevice: { _, _, _ in
            preconditionFailure("The menu fixture must not connect to a device")
        })
        model.connected = Device(id: "menu-ui-fixture", name: "Menu fixture device",
                                 selector: "menu-ui-fixture", transport: .lan, address: "")
        model.apps = [DeviceApp(appName: name, runningState: .stopped, services: [])]
        model.appActions = busy ? [name] : []
        _apps = StateObject(wrappedValue: model)
        let item = LocalSimulator(name: name, profile: "go2", state: "stopped", address: "",
                                  version: "fixture", error: "", diskBytes: 0)
        let simulatorModel = SimulatorModel(listSimulators: { [item] })
        simulatorModel.items = [item]
        simulatorModel.selection = item.id
        simulatorModel.operation = busy ? "Fixture operation in progress" : nil
        _simulators = StateObject(wrappedValue: simulatorModel)
    }

    var body: some View {
        VStack(spacing: 0) {
            if screen == "simulators" {
                SimulatorsView(model: simulators) { _ in
                    preconditionFailure("The menu fixture must not start a simulator connection")
                }
            } else {
                // Render the production table, including its constrained action
                // column, and observe the production action's model mutation.
                AppsView(model: apps, apps: apps.apps) { _ in
                    preconditionFailure("Unexpected logs action")
                }
                if let deletion = apps.appDeletion {
                    Text("Deletion requested: \(deletion.app)")
                        .accessibilityIdentifier("requested-app-deletion")
                    Button("Dismiss deletion request") { apps.appDeletion = nil }
                }
            }
        }
        .frame(width: narrow ? 900 : 1180, height: 620)
    }
}
