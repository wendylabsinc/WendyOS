import SwiftUI

/// Shared visual identity uses known device capabilities, never a guessed model.
struct DeviceEmblem: View {
    var lite = false
    var size: CGFloat = 64
    var completed = false
    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.28, style: .continuous)
                .fill((completed ? Color.green : Color.accentColor).gradient)
            RoundedRectangle(cornerRadius: size * 0.22, style: .continuous)
                .strokeBorder(.white.opacity(0.22), lineWidth: 1)
                .padding(size * 0.08)
            Image(systemName: completed ? "checkmark" : lite ? "sensor" : "cpu")
                .font(.system(size: size * 0.44, weight: .light)).foregroundStyle(.white)
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}

struct DeviceIdentityRow: View {
    let device: Device
    var connected = false
    var connecting = false
    var body: some View {
        HStack(spacing: 14) {
            DeviceEmblem(lite: device.lite, size: 46)
            VStack(alignment: .leading, spacing: 4) {
                Text(device.title).font(.headline).lineLimit(2)
                if connected { Text("Connected").font(.caption).foregroundStyle(.secondary) }
                else if device.lite { Text("Wendy Lite").font(.caption).foregroundStyle(.secondary) }
            }
            Spacer(minLength: 4)
            if connecting { ProgressView().controlSize(.small) }
            else if connected { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
        }.padding(.vertical, 6)
    }
}

enum ConnectionRecovery {
    static func canRetry(_ message: String) -> Bool {
        let value = message.lowercased()
        if ["permission", "unauthenticated", "sign in", "certificate", "identity", "pin mismatch", "not supported", "does not support", "update this device", "access denied"].contains(where: value.contains) { return false }
        return ["timeout", "timed out", "deadline", "unavailable", "no route", "network is unreachable", "network is down", "connection refused", "connection reset", "connection ended", "connection closed", "disconnected", "offline", "broken pipe", "startup interrupted", "eof"].contains(where: value.contains)
    }
}

struct DeviceConnectionView: View {
    @ObservedObject var model: AppModel
    let device: Device
    @State private var actions = AsyncActions()
    var body: some View {
        VStack(spacing: 22) {
            DeviceEmblem(lite: device.lite, size: 88)
            Text(device.title).font(.title2.weight(.semibold)).multilineTextAlignment(.center)
            if let error = model.detailError, !ConnectionRecovery.canRetry(error) {
                Text(PresentedFailure(error).message).foregroundStyle(.secondary).multilineTextAlignment(.center)
                if model.canForgetIdentity {
                    Button("Forget saved identity and reconnect") { actions.submit { await model.connect(device, forgetIdentity: true) } }
                } else {
                    Button("Check connection") { actions.submit { await model.retryConnection(device) } }
                }
            } else {
                ProgressView("Connecting…")
                Text("We’ll reconnect as soon as your device is available.")
                    .font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center)
            }
            if let error = model.detailError {
                DisclosureGroup("Connection details") {
                    Text(PresentedFailure(error).details).font(.caption.monospaced()).textSelection(.enabled)
                }.font(.caption).foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: 420).padding(28).frame(maxWidth: .infinity, maxHeight: .infinity)
        .task { await actions.run() }
    }
}

/// Hostnames are retained for addressing; the interface can show a readable name.
enum DeviceDisplayName {
    static func title(_ value: String) -> String {
        let prefixes = ["wendyos-", "wendy-lite-"]
        guard let prefix = prefixes.first(where: { value.lowercased().hasPrefix($0) }) else { return value }
        let suffix = String(value.dropFirst(prefix.count))
        guard !suffix.isEmpty else { return "Wendy device" }
        return suffix.split(separator: "-").map { word in
            let text = String(word)
            return text.contains(where: \.isNumber) ? text.uppercased() : text.capitalized
        }.joined(separator: " ")
    }
}
