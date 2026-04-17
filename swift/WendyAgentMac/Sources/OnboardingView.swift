import AppKit
import SwiftUI

struct OnboardingView: View {
    @Bindable var onboarding: Onboarding

    var body: some View {
        VStack(alignment: .leading, spacing: 24) {
            self.header

            Text("To finish setting up Wendy Agent, allow access to the hardware features below. Wendy apps can use Bluetooth, camera, and microphone once access is granted.")
                .font(.body)
                .fixedSize(horizontal: false, vertical: true)

            self.permissionsSection

            Toggle(
                "Open Wendy Agent automatically when you log in",
                isOn: Binding(
                    get: { self.onboarding.launchAtLoginEnabled },
                    set: { self.onboarding.setLaunchAtLoginEnabled($0) }
                )
            )
        }
        .padding(28)
        .frame(width: 620, alignment: .topLeading)
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            self.onboarding.refresh()
        }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 16) {
            Image(nsImage: NSApplication.shared.applicationIconImage)
                .resizable()
                .frame(width: 64, height: 64)
                .clipShape(RoundedRectangle(cornerRadius: 14))

            VStack(alignment: .leading, spacing: 6) {
                Text("Welcome to Wendy Agent")
                    .font(.largeTitle)
                    .fontWeight(.semibold)

                Text(AppDisplayName.current)
                    .font(.headline)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var permissionsSection: some View {
        VStack(alignment: .leading, spacing: 14) {
            ForEach(Onboarding.Permission.allCases, id: \.self) { permission in
                self.permissionRow(permission)
            }
        }
        .padding(18)
        .background(
            RoundedRectangle(cornerRadius: 14)
                .fill(.quaternary.opacity(0.4))
        )
    }

    @ViewBuilder
    private func permissionRow(_ permission: Onboarding.Permission) -> some View {
        let status = self.onboarding.status(for: permission)

        HStack(spacing: 12) {
            Image(systemName: permission.systemImage)
                .foregroundStyle(.secondary)
                .frame(width: 18)

            VStack(alignment: .leading, spacing: 2) {
                Text(permission.title)
                    .fontWeight(.medium)
                Text(permission.subtitle)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }

            Spacer()

            if self.onboarding.requestingPermission == permission {
                ProgressView()
                    .controlSize(.small)
                    .frame(minWidth: 72, alignment: .trailing)
            } else {
                self.permissionAction(status: status, permission: permission)
            }
        }
    }

    @ViewBuilder
    private func permissionAction(
        status: Onboarding.PermissionStatus,
        permission: Onboarding.Permission
    ) -> some View {
        switch status {
        case .pending:
            Button("Allow") {
                Task { @MainActor in
                    await self.onboarding.requestPermission(permission)
                }
            }
            .disabled(self.onboarding.isWorking)
        case .allowed:
            Label("Allowed", systemImage: "checkmark.circle.fill")
                .foregroundStyle(.green)
                .font(.subheadline)
        case .denied:
            Button("Open System Settings…") {
                self.onboarding.openSystemSettings(for: permission)
            }
            .disabled(self.onboarding.isWorking)
        case .restricted:
            Label("Restricted", systemImage: "xmark.circle.fill")
                .foregroundStyle(.red)
                .font(.subheadline)
        }
    }
}
