import SwiftUI
import AppKit

@main
struct WendyDesktopApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var model = AppModel()
    var body: some Scene {
        WindowGroup("Wendy", id: "main") {
            ContentView().environmentObject(model)
                .frame(minWidth: 900, minHeight: 620)
                .onAppear { delegate.model = model }
        }
        .defaultSize(width: 1180, height: 780)
        .commands {
            CommandGroup(replacing: .newItem) {}
            SidebarCommands()
            DesktopNavigationCommands()
        }
        Settings { AccountsSettingsView(model: model) }
        MenuBarExtra {
            VPNPopover(model: model, vpn: model.vpn, showsWindowActions: true)
        } label: {
            Label("Wendy VPN", systemImage: "network")
                .task { await model.cleanup.run() }
                .task(id: model.initialized) { await model.observeEvents() }
                .task(id: model.account) { await PushNotifications.shared.useAccount(model.account) }
                .task { await model.vpn.observeStatus() }
                .task { await delegate.observeTermination(model: model) }
        }.menuBarExtraStyle(.window)
    }
}

enum DesktopDevicePage: String, CaseIterable {
    case apps, cameras, siteModel, activity, campaigns, chat, data, settings

    var title: String { self == .siteModel ? "Digital Twin" : rawValue.capitalized }
    var shortcut: KeyEquivalent { KeyEquivalent(Character(String(Self.allCases.firstIndex(of: self)! + 1))) }

    static func adjacent(to current: String, forward: Bool) -> Self {
        guard let index = allCases.firstIndex(where: { $0.rawValue == current }) else { return .apps }
        return allCases[(index + (forward ? 1 : allCases.count - 1)) % allCases.count]
    }
}

struct DesktopNavigationActions {
    let deviceAvailable: Bool
    let findDevice, focusDevices, showOrganization, showSites: () -> Void
    let showSource: (String) -> Void
    let showPage: (DesktopDevicePage) -> Void
    let cyclePage: (Bool) -> Void
    let connectByAddress: () -> Void
}

private struct DesktopNavigationKey: FocusedValueKey { typealias Value = DesktopNavigationActions }
private struct DesktopBackKey: FocusedValueKey { typealias Value = () -> Void }
extension FocusedValues {
    var desktopNavigation: DesktopNavigationActions? {
        get { self[DesktopNavigationKey.self] }
        set { self[DesktopNavigationKey.self] = newValue }
    }
    var desktopBack: (() -> Void)? {
        get { self[DesktopBackKey.self] }
        set { self[DesktopBackKey.self] = newValue }
    }
}

struct DesktopNavigationCommands: Commands {
    @FocusedValue(\.desktopNavigation) private var navigation
    @FocusedValue(\.desktopBack) private var back

    // Scene values remain available behind a sheet. Do not navigate the window
    // underneath a modal dialog, including when a shortcut is invoked directly.
    private var modalPresented: Bool {
        NSApp.keyWindow?.attachedSheet != nil || NSApp.keyWindow?.sheetParent != nil
    }
    private func navigate(_ action: () -> Void) {
        guard !modalPresented else { return }
        action()
    }

    var body: some Commands {
        CommandMenu("Navigation") {
            Button("Back") { navigate { back?() } }
                .keyboardShortcut("[", modifiers: .command)
                .disabled(back == nil || modalPresented)
            Divider()
            Group {
                Button("Find a Device") { navigate { navigation?.findDevice() } }
                    .keyboardShortcut("f", modifiers: .command)
                Button("Focus Device List") { navigate { navigation?.focusDevices() } }
                    .keyboardShortcut("0", modifiers: .command)
                Button("Nearby Devices") { navigate { navigation?.showSource("nearby") } }
                    .keyboardShortcut("1", modifiers: [.command, .option])
                Button("Cloud Devices") { navigate { navigation?.showSource("cloud") } }
                    .keyboardShortcut("2", modifiers: [.command, .option])
                Button("Organization") { navigate { navigation?.showOrganization() } }
                    .keyboardShortcut("3", modifiers: [.command, .option])
                Button("Sites") { navigate { navigation?.showSites() } }
                    .keyboardShortcut("4", modifiers: [.command, .option])
                Button("Connect by Address…") { navigate { navigation?.connectByAddress() } }
                    .keyboardShortcut("k", modifiers: .command)
            }.disabled(navigation == nil || modalPresented)
            Divider()
            Group {
                ForEach(DesktopDevicePage.allCases, id: \.self) { page in
                    Button(page.title) { navigate { navigation?.showPage(page) } }
                        .keyboardShortcut(page.shortcut, modifiers: .command)
                }
                Divider()
                Button("Previous Device Page") { navigate { navigation?.cyclePage(false) } }
                    .keyboardShortcut("[", modifiers: [.command, .shift])
                Button("Next Device Page") { navigate { navigation?.cyclePage(true) } }
                    .keyboardShortcut("]", modifiers: [.command, .shift])
            }.disabled(navigation?.deviceAvailable != true || modalPresented)
        }
    }
}
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let termination = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
    func observeTermination(model: AppModel) async {
        for await _ in termination.stream {
            await model.shutdown()
            NSApplication.shared.reply(toApplicationShouldTerminate: true)
        }
    }
    weak var model: AppModel?
    func application(_ application: NSApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        PushNotifications.shared.didRegister(deviceToken)
    }
    func application(_ application: NSApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        PushNotifications.shared.didFail(error.localizedDescription)
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if InstallSession.shared.running {
            let alert = NSAlert()
            alert.messageText = "The installer is still running"
            alert.informativeText = "Return to the installer to finish or stop it before quitting Wendy. Interrupting a write may leave the device unable to boot."
            alert.addButton(withTitle: "Keep Wendy open")
            alert.runModal()
            return .terminateCancel
        }
        termination.continuation.yield(())
        return .terminateLater
    }
}
