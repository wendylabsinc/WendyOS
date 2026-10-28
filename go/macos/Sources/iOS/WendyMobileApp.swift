import SwiftUI
import UIKit

@main
struct WendyMobileApp: App {
    @UIApplicationDelegateAdaptor(MobileAppDelegate.self) private var delegate
    @StateObject private var model = AppModel()
    var body: some Scene {
        WindowGroup {
            MobileRootView().environmentObject(model)
                .task { await model.cleanup.run() }
                .task(id: model.initialized) { await model.observeEvents() }
                .task(id: model.account) { await PushNotifications.shared.useAccount(model.account) }
        }
    }
}

@MainActor
final class MobileAppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        PushNotifications.shared.didRegister(deviceToken)
    }
    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        PushNotifications.shared.didFail(error.localizedDescription)
    }
}
