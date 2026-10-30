import SwiftUI
#if os(macOS)
import AppKit
#else
import UIKit
#endif

struct NearbySetupPromptHistory {
    private var prompted: Set<String> = []

    mutating func next(_ devices: [NearbySetupDevice]) -> NearbySetupDevice? {
        guard let device = devices.filter({ (-80 ... -1).contains($0.rssi) && !prompted.contains($0.id) })
            .sorted(by: { $0.rssi == $1.rssi ? $0.id < $1.id : $0.rssi > $1.rssi }).first else { return nil }
        prompted.insert(device.id)
        return device
    }
}

/// Foreground discovery only. Advertising invites setup; enrollment still
/// requires the user's action and the setup service's live enrollment check.
struct NearbySetupPrompt: ViewModifier {
    @EnvironmentObject private var app: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @State private var presented: NearbySetupDevice?
    @State private var signInAfterDismiss = false
    @State private var actions = AsyncActions()

    private var active: Bool { app.initialized && scenePhase == .active }

    func body(content: Content) -> some View {
        content
            .sheet(item: $presented, onDismiss: {
                actions.submit {
                    if signInAfterDismiss { signInAfterDismiss = false; await app.signIn() }
                    await app.refreshNearby()
                }
            }) { device in
                NearbySetupInvitation(device: device) { signInAfterDismiss = true }
            }
            .task { await actions.run() }
            .task(id: active) {
                guard active else { return }
                while !Task.isCancelled {
                    if presented == nil && !anotherPresentation {
                        let devices = (try? await GoClient.shared.discoverSetupDevices()) ?? []
                        guard !Task.isCancelled, active else { return }
                        if presented == nil && !anotherPresentation {
                            presented = app.nearbySetupPromptHistory.next(devices)
                        }
                    }
                    do { try await Task.sleep(for: .seconds(5)) } catch { return }
                }
            }
    }

    // A setup prompt must not interrupt another sheet, sign-in, or confirmation.
    private var anotherPresentation: Bool {
        if app.signingIn || app.loginURL != nil || app.alert != nil || app.appDeletion != nil { return true }
        #if os(macOS)
        return NSApp.keyWindow?.attachedSheet != nil || NSApp.keyWindow?.sheetParent != nil
        #else
        return UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
            .flatMap(\.windows).contains { $0.isKeyWindow && $0.rootViewController?.presentedViewController != nil }
        #endif
    }
}

private struct NearbySetupInvitation: View {
    @Environment(\.dismiss) private var dismiss
    let device: NearbySetupDevice
    let signIn: () -> Void
    @State private var configuring = false

    var body: some View {
        if configuring {
            NearbySetupView(signIn: signIn, initialDevice: device)
        } else {
            VStack(spacing: 20) {
                Image(systemName: "antenna.radiowaves.left.and.right").font(.system(size: 44)).foregroundStyle(.tint)
                Text("Enroll this device?").font(.title2.bold())
                Text(device.title).font(.headline)
                Text("An unenrolled Wendy device is nearby. Set it up to add it to your organization.")
                    .foregroundStyle(.secondary).multilineTextAlignment(.center)
                Button("Set up device") { configuring = true }.buttonStyle(.borderedProminent)
                Button("Not now", role: .cancel) { dismiss() }
            }
            .padding(32)
            #if os(macOS)
            .frame(width: 380)
            #else
            .presentationDetents([.medium])
            #endif
        }
    }
}
