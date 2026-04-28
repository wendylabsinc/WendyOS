import AppKit
import SwiftUI

struct WelcomeAndPermissionsView: View {
    @Bindable var welcomeAndPermissions: WelcomeAndPermissions
    let onPermissionRequestCompleted: () -> Void

    private let betaRibbonYellow = Color(
        red: 251.0 / 255.0,
        green: 191.0 / 255.0,
        blue: 36.0 / 255.0
    )

    var body: some View {
        VStack(alignment: .leading, spacing: 24) {
            self.header

            Text(
                "To finish setting up Wendy Agent, allow access to the hardware features below. Wendy apps can use Bluetooth, camera, and microphone once access is granted."
            )
            .font(.body)
            .fixedSize(horizontal: false, vertical: true)

            self.betaNotice

            self.permissionsSection

            Toggle(
                "Open Wendy Agent automatically when you log in",
                isOn: Binding(
                    get: { self.welcomeAndPermissions.launchAtLoginEnabled },
                    set: { self.welcomeAndPermissions.setLaunchAtLoginEnabled($0) }
                )
            )
        }
        .padding(28)
        .frame(width: 620, alignment: .topLeading)
        .onReceive(
            NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)
        ) { _ in
            self.welcomeAndPermissions.refresh()
        }
    }

    private var header: some View {
        HStack(alignment: .center, spacing: 16) {
            Image(nsImage: NSApplication.shared.applicationIconImage)
                .resizable()
                .frame(width: 64, height: 64)
                .clipShape(RoundedRectangle(cornerRadius: 14))

            VStack(alignment: .leading, spacing: 6) {
                Text("Welcome to Wendy Agent")
                    .font(.largeTitle)
                    .fontWeight(.semibold)
            }
        }
    }

    private var betaNotice: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(self.betaRibbonYellow)
                .font(.title3)
                .frame(width: 18)

            VStack(alignment: .leading, spacing: 4) {
                Text("Beta Software")
                    .fontWeight(.semibold)

                Text(
                    "Wendy Agent is prerelease software and may change or be incomplete. It is not intended for production use."
                )
                .font(.body)
                .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(18)
        .background(
            RoundedRectangle(cornerRadius: 14)
                .fill(self.betaRibbonYellow.opacity(0.10))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 14)
                .stroke(self.betaRibbonYellow.opacity(0.18), lineWidth: 1)
        )
    }

    private var permissionsSection: some View {
        VStack(alignment: .leading, spacing: 14) {
            ForEach(WelcomeAndPermissions.Permission.allCases, id: \.self) { permission in
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
    private func permissionRow(_ permission: WelcomeAndPermissions.Permission) -> some View {
        let status = self.welcomeAndPermissions.status(for: permission)

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

            if self.welcomeAndPermissions.requestingPermission == permission {
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
        status: WelcomeAndPermissions.PermissionStatus,
        permission: WelcomeAndPermissions.Permission
    ) -> some View {
        switch status {
        case .pending:
            Button("Allow") {
                Task { @MainActor in
                    await self.welcomeAndPermissions.requestPermission(permission)
                    self.onPermissionRequestCompleted()
                }
            }
            .disabled(self.welcomeAndPermissions.isWorking)
        case .allowed:
            Label("Allowed", systemImage: "checkmark.circle.fill")
                .foregroundStyle(.green)
                .font(.subheadline)
        case .denied:
            Button("Open System Settings…") {
                self.welcomeAndPermissions.openSystemSettings(for: permission)
            }
            .disabled(self.welcomeAndPermissions.isWorking)
        case .restricted:
            Label("Restricted", systemImage: "xmark.circle.fill")
                .foregroundStyle(.red)
                .font(.subheadline)
        }
    }
}
