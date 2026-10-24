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
        .commands { CommandGroup(replacing: .newItem) {} }
        Settings { AccountsSettingsView(model: model) }
        MenuBarExtra("Wendy VPN", systemImage: "network") {
            VPNPopover(model: model, vpn: model.vpn, showsWindowActions: true)
        }.menuBarExtraStyle(.window)
    }
}
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    weak var model: AppModel?
    func application(_ application: NSApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        PushNotifications.shared.didRegister(deviceToken)
    }
    func application(_ application: NSApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        PushNotifications.shared.didFail(error.localizedDescription)
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        Task { await model?.shutdown(); sender.reply(toApplicationShouldTerminate: true) }
        return .terminateLater
    }
}
