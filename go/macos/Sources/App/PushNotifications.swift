import SwiftUI
import UserNotifications
#if os(macOS)
import AppKit
import Security
#else
import UIKit
#endif

extension GoClient {
    func pushRegistration(account: String, token: Data, topic: String, environment: Int32, enabled: Bool) async throws {
        try await perform {
            try Self.withCString(account) { account in
                try Self.withCString(topic) { topic in
                    try token.withUnsafeBytes { bytes in
                        try Self.finish(WDPushRegistration(account, UnsafeMutablePointer(mutating: bytes.bindMemory(to: UInt8.self).baseAddress), bytes.count, topic, environment, enabled ? 1 : 0))
                    }
                }
            }
        }
    }
}

@MainActor
final class PushNotifications: NSObject, ObservableObject, UNUserNotificationCenterDelegate {
    static let shared = PushNotifications()
    @Published private(set) var status = "Enable notifications to receive Wendy Cloud alerts here."
    @Published private(set) var enabled = false
    @Published private(set) var busy = false
    @Published private(set) var denied = false
    @Published private(set) var failure: String?
    private var account: CloudAccount?
    private var token: Data?
    private var generation = 0
    private var registration: Task<Void, Never>?
    private var timeout: Task<Void, Never>?
    private var preferenceKey: String? {
        guard let account, account.signedIn else { return nil }
        return "pushNotifications." + account.cloud + "." + account.context + "." + account.email
    }

    override init() {
        super.init()
        UNUserNotificationCenter.current().delegate = self
    }

    func useAccount(_ next: CloudAccount?) async {
        let previousIdentity = preferenceKey
        account = next
        guard previousIdentity != preferenceKey else { return }
        generation += 1
        registration?.cancel(); timeout?.cancel()
        enabled = false; busy = false; failure = nil
        status = "Enable notifications to receive Wendy Cloud alerts here."
        guard let key = preferenceKey else { status = "Sign in to receive Cloud notifications."; return }
        if UserDefaults.standard.bool(forKey: key) { await enable(requestPermission: false) }
    }

    func refreshAuthorization() async {
        let settings = await UNUserNotificationCenter.current().notificationSettings()
        denied = settings.authorizationStatus == .denied
        if denied {
            enabled = false
            status = "Notifications are disabled in system settings."
        } else if !busy, !enabled, let key = preferenceKey, UserDefaults.standard.bool(forKey: key), settings.authorizationStatus == .authorized {
            await enable(requestPermission: false)
        }
    }

    func enable(requestPermission: Bool = true) async {
        guard !busy, let key = preferenceKey else { return }
        busy = true; failure = nil
        let current = generation
        do {
            let center = UNUserNotificationCenter.current()
            var settings = await center.notificationSettings()
            if settings.authorizationStatus == .notDetermined && requestPermission {
                _ = try await center.requestAuthorization(options: [.alert, .sound, .badge])
                settings = await center.notificationSettings()
            }
            guard current == generation else { return }
            denied = settings.authorizationStatus == .denied
            guard settings.authorizationStatus == .authorized || settings.authorizationStatus == .provisional else {
                busy = false
                status = denied ? "Notifications are disabled in system settings." : "Notification permission has not been granted."
                return
            }
            UserDefaults.standard.set(true, forKey: key)
            status = "Registering with Apple…"
            #if os(macOS)
            NSApplication.shared.registerForRemoteNotifications()
            #else
            UIApplication.shared.registerForRemoteNotifications()
            #endif
            timeout?.cancel()
            timeout = Task { [weak self] in
                try? await Task.sleep(for: .seconds(30))
                guard !Task.isCancelled, let self, current == self.generation, self.busy else { return }
                self.didFail("Apple push registration timed out. Try again when connected to the internet.")
            }
        } catch { if current == generation { didFail(error.localizedDescription) } }
    }

    func didRegister(_ token: Data) {
        self.token = token
        guard let account, let key = preferenceKey, UserDefaults.standard.bool(forKey: key) else { busy = false; return }
        registration?.cancel()
        let current = generation
        status = "Registering with Wendy Cloud…"
        registration = Task {
            do {
                let environment = try Self.pushEnvironment()
                guard let topic = Bundle.main.bundleIdentifier else { throw WendyError(message: "Missing application identity.") }
                try await GoClient.shared.pushRegistration(account: account.context, token: token, topic: topic, environment: environment, enabled: true)
                guard !Task.isCancelled, current == generation else { return }
                timeout?.cancel(); busy = false; enabled = true; failure = nil
                status = "Receiving Cloud notifications for \(account.email.isEmpty ? account.context : account.email)."
            } catch { if !Task.isCancelled && current == generation { didFail(error.localizedDescription) } }
        }
    }

    func didFail(_ message: String) {
        timeout?.cancel()
        busy = false; enabled = false
        status = "Cloud notifications are not registered."
        failure = message
    }

    func disable() async {
        guard !busy, let account, let key = preferenceKey, let token else { return }
        busy = true; failure = nil
        let current = generation
        do {
            try await GoClient.shared.pushRegistration(account: account.context, token: token, topic: Bundle.main.bundleIdentifier ?? "", environment: Self.pushEnvironment(), enabled: false)
            guard current == generation else { return }
            UserDefaults.standard.set(false, forKey: key)
            enabled = false; busy = false
            status = "Cloud notifications are turned off on this device."
        } catch { if current == generation { busy = false; failure = error.localizedDescription } }
    }

    private static func pushEnvironment() throws -> Int32 {
        #if os(macOS)
        let task = SecTaskCreateFromSelf(nil)
        let value = task.flatMap { SecTaskCopyValueForEntitlement($0, "com.apple.developer.aps-environment" as CFString, nil) as? String }
        #else
        let value = Bundle.main.object(forInfoDictionaryKey: "WendyPushEnvironment") as? String
        #endif
        switch value {
        case "development": return 1
        case "production": return 2
        default: throw WendyError(message: "This build needs an Apple push-enabled signing profile. Rebuild with Push Notifications enabled for this app ID.")
        }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification, withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .list, .sound])
    }
}

struct NotificationReceivingView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @ObservedObject private var push = PushNotifications.shared
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(push.status).font(.callout)
            if let failure = push.failure { Text(failure).font(.caption).foregroundStyle(.secondary).textSelection(.enabled) }
            if push.busy {
                ProgressView("Setting up notifications…").controlSize(.small)
            } else if push.denied {
                Button("Open notification settings") { openSystemSettings() }
            } else if push.enabled {
                Button("Turn off on this device") { Task { await push.disable() } }
            } else {
                Button("Enable notifications on this device") {
                    Task { await push.useAccount(model.account); await push.enable() }
                }.disabled(model.account?.signedIn != true)
            }
        }.task { await push.useAccount(model.account); await push.refreshAuthorization() }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { Task { await push.refreshAuthorization() } }
        }
    }
    private func openSystemSettings() {
        #if os(macOS)
        if let url = URL(string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension") { NSWorkspace.shared.open(url) }
        #else
        if let url = URL(string: UIApplication.openNotificationSettingsURLString) { UIApplication.shared.open(url) }
        #endif
    }
}
