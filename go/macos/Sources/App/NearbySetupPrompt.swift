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
    @Environment(\.dynamicTypeSize) private var typeSize
    let device: NearbySetupDevice
    let signIn: () -> Void
    @State private var configuring = false

    var body: some View {
        if configuring {
            NearbySetupView(signIn: signIn, initialDevice: device)
                #if os(iOS)
                .presentationDetents([.large])
                #endif
        } else {
            ScrollView {
                VStack(spacing: 24) {
                    ZStack {
                        Circle().fill(Color.accentColor.opacity(0.04)).frame(width: 164, height: 164)
                        Circle().strokeBorder(Color.accentColor.opacity(0.10), lineWidth: 1).frame(width: 142, height: 142)
                        RoundedRectangle(cornerRadius: 28, style: .continuous)
                            .fill(Color.accentColor.gradient).frame(width: 100, height: 100)
                            .shadow(color: Color.accentColor.opacity(0.2), radius: 18, y: 8)
                        Image(systemName: "cpu").font(.system(size: 46, weight: .light)).foregroundStyle(.white)
                    }.accessibilityHidden(true)
                    VStack(spacing: 8) {
                        Text("A Wendy is nearby").font(.title2.weight(.semibold))
                        Text(device.title).font(.subheadline).foregroundStyle(.secondary)
                            .multilineTextAlignment(.center).textSelection(.enabled)
                        Text("Add it to your organization to make it yours.")
                            .font(.callout).foregroundStyle(.secondary)
                            .multilineTextAlignment(.center).padding(.top, 4)
                    }
                    VStack(spacing: 8) {
                        Button { configuring = true } label: {
                            HStack {
                                Spacer()
                                Text("Set up device").fontWeight(.semibold)
                                Image(systemName: "arrow.right").font(.callout.weight(.semibold))
                                Spacer()
                            }.padding(.vertical, 8)
                        }.buttonStyle(.borderedProminent).controlSize(.large)
                        Button("Not now", role: .cancel) { dismiss() }
                            .buttonStyle(.plain).foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                }.padding(.horizontal, 28).padding(.top, 20).padding(.bottom, 12)
            }
            .scrollBounceBehavior(.basedOnSize)
            #if os(macOS)
            .frame(width: 400, height: 470)
            #else
            .presentationDetents(typeSize.isAccessibilitySize ? [.large] : [.height(470)])
            .presentationDragIndicator(.visible)
            .presentationCornerRadius(36)
            #endif
        }
    }
}
