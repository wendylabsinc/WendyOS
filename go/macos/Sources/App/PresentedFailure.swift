import SwiftUI

struct PresentedFailure {
    private(set) var title: String? = nil
    private(set) var symbol = "wifi.exclamationmark"
    let message: String
    let details: String

    init(_ raw: String) {
        let lower = raw.lowercased()
        if lower.contains("valid app_id is required") {
            message = "Cloud rejected this campaign’s notification identity. Update the device agent, then enable campaign notifications again."
        } else if lower.contains("does not support direct online updates yet") {
            message = "This agent needs a newer version before it can download updates directly."
        } else if lower.contains("select at least one device") {
            message = "Select at least one device, then turn on VPN."
        } else if lower.contains("not sending video") {
            message = "This camera is not sending video. Check its source, or choose another camera."
        } else if lower.contains("no route to host") || lower.contains("deadline exceeded") || lower.contains("connection refused") || lower.contains("offline") {
            message = "This device could not be reached. Check that it is on and connected, then try again."
        } else if lower.contains("not logged in") || lower.contains("no login") || lower.contains("sign in") || lower.contains("unauthenticated") {
            message = "Sign in to Wendy Cloud, then try again."
        } else if lower.contains("identity changed") || lower.contains("different certificate key") || lower.contains("has no enrolled identity") {
            title = "Connection blocked"
            symbol = "exclamationmark.shield"
            message = "This device does not match the identity saved on this Mac. Check the selected account and refresh Cloud devices, then try again. If you reset or re-enrolled it, forget its saved identity and reconnect."
        } else if lower.contains("pin mismatch") || lower.contains("identity mismatch") || lower.contains("certificate") {
            title = "Connection blocked"
            symbol = "exclamationmark.shield"
            message = "Wendy could not verify this device. Check its identity and your sign-in before reconnecting."
        } else if lower.contains("permissiondenied") || lower.contains("permission denied") {
            message = "Your account does not have access to this device. Check the selected organization and try again."
        } else if lower.contains("systemextension") || lower.contains("system extension") || lower.contains("code signature") {
            message = "The network extension could not start. Use a signed build in Applications and allow its network extension in System Settings."
        } else {
            message = "Wendy could not complete this request. Try again."
        }
        // Diagnostics remain available without putting credentials into alerts.
        var safe = raw
        for pattern in [
            "(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----",
            "(?i)(?:Bearer|DPoP)\\s+[A-Za-z0-9._~+/=-]+",
            "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+",
            "(?i)(?:access_token|refresh_token|api_key|token)=([^&\\s]+)"
        ] {
            safe = safe.replacingOccurrences(of: pattern, with: "[redacted]", options: .regularExpression)
        }
        details = String(safe.prefix(4000))
    }
}

struct FailureView: View {
    let title: String
    let error: String
    var forgetIdentity: (() -> Void)? = nil
    let retry: () -> Void
    var body: some View {
        let failure = PresentedFailure(error)
        VStack(spacing: 16) {
            Spacer()
            Image(systemName: failure.symbol).font(.system(size: 42)).foregroundStyle(.tertiary)
            Text(failure.title ?? title).font(.title2.bold())
            Text(failure.message).foregroundStyle(.secondary).multilineTextAlignment(.center).frame(maxWidth: 420)
            Button("Try again", action: retry)
            if let forgetIdentity {
                Button("Forget saved identity and retry", action: forgetIdentity)
                Text("Clears this device’s saved identity on this Mac.").font(.caption).foregroundStyle(.secondary)
            }
            DisclosureGroup("Technical details") {
                ScrollView {
                    Text(failure.details).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading).padding(.vertical, 8)
                }.frame(maxHeight: 160)
            }.font(.caption).foregroundStyle(.secondary).frame(maxWidth: 520)
            Spacer()
        }.padding(28).frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
struct FailureNote: View {
    let message: String
    var body: some View {
        let failure = PresentedFailure(message)
        DisclosureGroup {
            Text(failure.details).font(.system(.caption2, design: .monospaced)).textSelection(.enabled)
        } label: { Text(failure.message) }
        .font(.caption).foregroundStyle(.secondary)
    }
}
