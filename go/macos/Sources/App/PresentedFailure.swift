import SwiftUI
import Network
#if os(iOS)
import UIKit
#endif

struct PresentedFailure {
    private(set) var title: String? = nil
    private(set) var symbol = "wifi.exclamationmark"
    private(set) var offersDeviceSignIn = false
    private(set) var offersNetworkSettings = false
    let message: String
    let details: String

    // Typed bridge errors never fall back to text matching, even when unknown.
    init(_ error: WendyError) {
        details = Self.redact(error.message)
        offersDeviceSignIn = error.remedy == .signIn
        offersNetworkSettings = error.remedy == .openNetworkSettings
        switch error.code {
        case .authenticationRequired:
            title = "Sign in to connect"
            message = "Sign in to Wendy Cloud with an account in this device's organization, then try again."
        case .credentialsExpired:
            title = "Sign in again"
            message = "Your device sign-in needs to be renewed. Check that your date and time are correct, then sign in again."
        case .accountMismatch, .accessDenied:
            title = "Check your account"
            message = "Choose the device's account and organization in Accounts. If you still cannot connect, ask your organization administrator for access."
        case .identityMismatch:
            title = "Device identity changed"
            symbol = "exclamationmark.shield"
            message = "This device does not match its saved identity. Check the account and device you selected. If you reset or re-enrolled the device, forget its saved identity and reconnect."
        case .secureConnectionRejected:
            title = "Check the device clock"
            symbol = "exclamationmark.shield"
            message = "The device rejected a secure connection. Check the date and time on both devices. Connect the device to the internet so it can set its clock, then try again."
        case .networkUnavailable:
            title = "Connection unavailable"
            message = "Check that the device is powered on and online. For a nearby device, connect to the same network, then try again."
        case .localNetworkDenied:
            title = "Allow local network access"
            message = "Allow Wendy to access your local network in Settings, then try again."
        case .agentUpdateRequired:
            title = "Update the device"
            message = "This feature needs a newer Wendy Agent. Open the device settings and update the agent, then try again."
        case .storageFull:
            title = "Storage is full"
            message = "Remove unused apps, models, or recordings from the device, then try again."
        case .portInUse:
            title = "Port already in use"
            message = "Choose another port, or stop the app using this port, then try again."
        case .serviceBusy:
            title = "Service is busy"
            message = "Wait a moment, then try again. If this keeps happening, check your organization's usage limits."
        case .cancelled:
            title = "Request cancelled"
            message = "The request was cancelled. You can try again when you are ready."
        case .unknown:
            message = "Wendy could not complete this request. Try again."
        }
    }

    // Legacy screens still receive plain strings. Bridge errors must use the typed initializer.
    init(_ raw: String) {
        let lower = raw.lowercased()
        if lower.contains("available client certificates are expired") || lower.contains("certificate is not valid for client authentication") || (lower.contains("your wendy login") && lower.contains("expired")) {
            title = "Sign in again"
            symbol = "person.crop.circle.badge.exclamationmark"
            message = "Your device sign-in has expired or needs to be renewed. Check that your date and time are correct, then sign in again with an account in this device's organization."
            offersDeviceSignIn = true
        } else if lower.contains("device is not in the device list yet") {
            message = "Your device has not appeared in the device list yet. Keep it online, then open it again."
        } else if lower.contains("device name is already in use") {
            message = "That device name is already in use. Choose another name."
        } else if lower.contains("cloud rejected the enrollment details") {
            message = "Check the device name and organization."
        } else if lower.contains("does not have permission to add devices") {
            message = "Your account cannot add devices to this organization. Choose another organization or ask an administrator for access."
        } else if lower.contains("enrollment is not available for this organization") {
            message = "Enrollment is not available for this organization. Contact your administrator."
        } else if lower.contains("sign in to wendy cloud to connect to this enrolled device") {
            title = "Sign in to connect"
            message = "This device is enrolled. Sign in to Wendy Cloud with an account in its organization."
            offersDeviceSignIn = true
        } else if lower.contains("this account has no device connection certificate") {
            title = "Device certificate needed"
            message = "Your signed-in account has no device connection certificate. Sign in again with an account in the device’s organization."
            offersDeviceSignIn = true
        } else if lower.contains("local network access denied") {
            title = "Allow local network access"
            offersNetworkSettings = true
            #if os(iOS)
            message = "Allow Wendy in Settings → Privacy & Security → Local Network, then try again."
            #else
            message = "macOS is blocking Wendy from reaching this device. Allow Wendy in Local Network settings, then find the device again."
            #endif
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
        } else if lower.contains("sends vp8 video") {
            message = "This device sends VP8 video, which the Wendy app cannot play yet. Update the device’s Wendy agent, or install gstreamer1.0-plugins-bad on it."
        } else if lower.contains("identity changed") || lower.contains("different certificate key") || lower.contains("has no enrolled identity") || lower.contains("pin mismatch") || lower.contains("identity mismatch") {
            title = "Device identity changed"
            symbol = "exclamationmark.shield"
            message = "This device does not match its saved identity. Check the account and device you selected. If you reset or re-enrolled the device, forget its saved identity and reconnect."
        } else if lower.contains("not logged in") || lower.contains("no login") || lower.contains("sign in") || lower.contains("unauthenticated") || lower.contains("invalid_grant") || lower.contains("token expired") {
            title = "Sign in again"
            message = "Sign in to Wendy Cloud with an account in this device's organization, then try again."
            offersDeviceSignIn = true
        } else if lower.contains("permissiondenied") || lower.contains("permission denied") || lower.contains("outside the current cloud organization") || lower.contains("choose the account for this enrolled device") || lower.contains("different cloud servers") {
            title = "Check your account"
            message = "Choose the device's account and organization in Accounts. If you still cannot connect, ask your organization administrator for access."
        } else if lower.contains("tls authentication failed") || lower.contains("certificate") {
            title = "Check the device clock"
            symbol = "exclamationmark.shield"
            message = "The device rejected a secure connection. Check the date and time on both devices. Connect the device to the internet so it can set its clock, then try again."
        } else if lower.contains("no route to host") || lower.contains("deadline exceeded") || lower.contains("connection refused") || lower.contains("offline") || lower.contains("network is unreachable") || lower.contains("no such host") || lower.contains("timed out") {
            title = "Device unreachable"
            message = "Check that the device is powered on and online. For a nearby device, connect to the same network, then try again."
        } else if lower.contains("no space left") || lower.contains("disk full") {
            title = "Storage is full"
            message = "Remove unused apps, models, or recordings from the device, then try again."
        } else if lower.contains("port is already in use") || lower.contains("address already in use") {
            title = "Port already in use"
            message = "Choose another port, or stop the app using this port, then try again."
        } else if lower.contains("unimplemented") || lower.contains("update the agent") || lower.contains("updated wendy agent") {
            title = "Update the device"
            message = "This feature needs a newer Wendy Agent. Open the device settings and update the agent, then try again."
        } else if lower.contains("resourceexhausted") || lower.contains("too many requests") {
            title = "Service is busy"
            message = "Wait a moment, then try again. If this keeps happening, check your organization's usage limits."
        } else if lower.contains("unavailable") {
            title = "Service unavailable"
            message = "Check your internet connection, then try again in a moment."
        } else if lower.contains("systemextension") || lower.contains("system extension") || lower.contains("code signature") {
            message = "The network extension could not start. Use a signed build in Applications and allow its network extension in System Settings."
        } else {
            message = "Wendy could not complete this request. Try again."
        }
        details = Self.redact(raw)
    }

    private static func redact(_ raw: String) -> String {
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
        return String(safe.prefix(4000))
    }
}

private struct FailureSignInKey: EnvironmentKey {
    static let defaultValue: (@MainActor () -> Void)? = nil
}

extension EnvironmentValues {
    var failureSignIn: (@MainActor () -> Void)? {
        get { self[FailureSignInKey.self] }
        set { self[FailureSignInKey.self] = newValue }
    }
}

struct FailureView: View {
    let title: String
    private let failure: PresentedFailure
    var retryLabel = "Try again"
    var forgetIdentity: (() -> Void)? = nil
    let retry: () -> Void
    init(title: String, error: WendyError, retryLabel: String = "Try again", forgetIdentity: (() -> Void)? = nil, retry: @escaping () -> Void) {
        self.title = title; failure = PresentedFailure(error); self.retryLabel = retryLabel
        self.forgetIdentity = forgetIdentity; self.retry = retry
    }
    init(title: String, error: String, retryLabel: String = "Try again", forgetIdentity: (() -> Void)? = nil, retry: @escaping () -> Void) {
        self.title = title; failure = PresentedFailure(error); self.retryLabel = retryLabel
        self.forgetIdentity = forgetIdentity; self.retry = retry
    }
    @Environment(\.failureSignIn) private var signIn
    @State private var showingDetails = false
    @Environment(\.openURL) private var openURL
    var body: some View {
        VStack(spacing: 16) {
            Spacer()
            Image(systemName: failure.symbol).font(.system(size: 42)).foregroundStyle(.tertiary)
            Text(failure.title ?? title).font(.title2.bold())
            Text(failure.message).foregroundStyle(.secondary).multilineTextAlignment(.center).frame(maxWidth: 420)
            if failure.offersDeviceSignIn, let signIn {
                Button("Sign in again", action: signIn)
            } else {
                Button(retryLabel, action: retry)
            }
            if failure.offersNetworkSettings {
                Button("Open Settings") {
                    #if os(iOS)
                    if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
                    #else
                    if let url = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork") { openURL(url) }
                    #endif
                }
            }
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
    private let failure: PresentedFailure
    init(message: WendyError) { failure = PresentedFailure(message) }
    init(message: String) { failure = PresentedFailure(message) }
    @Environment(\.failureSignIn) private var signIn
    var body: some View {
        DisclosureGroup {
            Text(failure.details).font(.system(.caption2, design: .monospaced)).textSelection(.enabled)
        } label: { Text(failure.message) }
        .font(.caption).foregroundStyle(.secondary)
        if failure.offersDeviceSignIn, let signIn {
            Button("Sign in again", action: signIn).font(.caption)
        }
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
