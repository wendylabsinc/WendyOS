import SwiftUI
import Network

struct PresentedFailure {
    private(set) var title: String? = nil
    private(set) var symbol = "wifi.exclamationmark"
    let message: String
    let details: String

    init(_ raw: String) {
        let lower = raw.lowercased()
        if lower.contains("local network access denied") {
            title = "Allow local network access"
            message = "macOS is blocking Wendy from reaching this device. Allow Wendy in Local Network settings, then find the device again."
        } else if lower.contains("agent does not support network traffic statistics") {
            message = "This device’s agent does not report network traffic. Update the agent to see these statistics."
        } else if lower.contains("valid app_id is required") {
            message = "Cloud rejected this campaign’s notification identity. Update the device agent, then enable campaign notifications again."
        } else if lower.contains("does not support direct online updates yet") {
            message = "This agent needs a newer version before it can download updates directly."
        } else if lower.contains("select at least one device") {
            message = "Select at least one device, then turn on VPN."
        } else if lower.contains("cloud does not offer tunnel service") {
            message = "This Cloud server needs an update before app ports can be reached through the VPN."
        } else if lower.contains("not sending video") {
            message = "This camera is not sending video. Check its source, or choose another camera."
        } else if lower.contains("no route to host") || lower.contains("deadline exceeded") || lower.contains("connection refused") || lower.contains("offline") {
            message = "The device could not be reached. Find it again to refresh its address. Its saved identity will be kept."
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
    var retryLabel = "Try again"
    var forgetIdentity: (() -> Void)? = nil
    let retry: () -> Void
    @State private var showingDetails = false
    @Environment(\.openURL) private var openURL
    var body: some View {
        let failure = PresentedFailure(error)
        VStack(spacing: 16) {
            Spacer()
            Image(systemName: failure.symbol).font(.system(size: 42)).foregroundStyle(.tertiary)
            Text(failure.title ?? title).font(.title2.bold())
            Text(failure.message).foregroundStyle(.secondary).multilineTextAlignment(.center).frame(maxWidth: 420)
            Button(retryLabel, action: retry)
            #if os(macOS)
            if error.lowercased().contains("no route to host") || error.lowercased().contains("local network access denied") {
                Button("Local Network settings") {
                    if let url = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork") { openURL(url) }
                }
            }
            #endif
            if let forgetIdentity {
                Button("Forget saved identity and retry", action: forgetIdentity)
                Text("Clears this device’s saved identity on this Mac.").font(.caption).foregroundStyle(.secondary)
            }
            Button("Technical details") { showingDetails = true }
                .font(.caption)
                .sheet(isPresented: $showingDetails) {
                    VStack(alignment: .leading, spacing: 16) {
                        Text("Connection details").font(.title2)
                        ScrollView {
                            Text(failure.details).font(.system(.caption, design: .monospaced))
                                .textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                        }.frame(height: 260)
                        HStack { Spacer(); Button("Done") { showingDetails = false }.keyboardShortcut(.defaultAction) }
                    }.padding(24).frame(width: 560)
                }
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

// Network.framework distinguishes a privacy denial from ordinary unreachable
// addresses; a BSD socket's EHOSTUNREACH cannot tell the UI which one occurred.
enum LocalNetworkAccess {
    static func denied(selector: String) async -> Bool {
        guard !selector.contains("://"), let separator = selector.lastIndex(of: ":"),
              let portNumber = UInt16(selector[selector.index(after: separator)...]),
              let port = NWEndpoint.Port(rawValue: portNumber) else { return false }
        let host = String(selector[..<separator]).trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        let connection = NWConnection(host: NWEndpoint.Host(host), port: port, using: .tcp)
        let events = AsyncStream<Bool>.makeStream(bufferingPolicy: .bufferingNewest(1))
        connection.stateUpdateHandler = { state in
            switch state {
            case .waiting:
                events.continuation.yield(connection.currentPath?.unsatisfiedReason == .localNetworkDenied)
            case .ready, .failed, .cancelled: events.continuation.yield(false)
            default: break
            }
        }
        async let _: Void = expire(events.continuation)
        defer { connection.cancel(); events.continuation.finish() }
        connection.start(queue: .global(qos: .utility))
        for await denied in events.stream { return denied }
        return false
    }
    private static func expire(_ continuation: AsyncStream<Bool>.Continuation) async {
        do { try await Task.sleep(for: .seconds(3)); continuation.yield(false) } catch { }
    }
}
