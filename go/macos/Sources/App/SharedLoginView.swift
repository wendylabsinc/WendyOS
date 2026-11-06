import SwiftUI
import Security

private struct SharedLoginFailure: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

enum SharedLoginKeychain {
    private static let queue = DispatchQueue(label: "dev.wendy.shared-login")
    private static func query() throws -> [String: Any] {
        guard let group = Bundle.main.object(forInfoDictionaryKey: "WendySharedKeychainGroup") as? String,
              !group.isEmpty, !group.contains("$(") else {
            throw SharedLoginFailure(message: "This build has no shared Keychain group. Install a signed Wendy build with Keychain Sharing enabled.")
        }
        return [kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: "dev.wendy.shared-login.v1",
                kSecAttrAccount as String: "current",
                kSecAttrAccessGroup as String: group,
                kSecAttrSynchronizable as String: true,
                kSecUseDataProtectionKeychain as String: true]
    }
    private static func check(_ status: OSStatus) throws {
        guard status != errSecSuccess else { return }
        if status == errSecMissingEntitlement {
            throw SharedLoginFailure(message: "This app's signing profile does not allow the shared Keychain group. Install a Wendy Labs signed build.")
        }
        throw SharedLoginFailure(message: (SecCopyErrorMessageString(status, nil) as String?) ?? "Keychain error \(status)")
    }
    static func publish() async throws {
        try await BlockingWork.run(on: queue) {
            guard let result = WDSharedLoginExport() else { throw SharedLoginFailure(message: "Could not read the current login.") }
            defer { WDSharedLoginFree(result) }
            if let text = result.pointee.status.message { throw SharedLoginFailure(message: String(cString: text)) }
            guard let bytes = result.pointee.data, result.pointee.count > 0, result.pointee.count <= 65_536 else {
                throw SharedLoginFailure(message: "The shared login is invalid or too large.")
            }
            var data = Data(bytes: bytes, count: result.pointee.count)
            defer { data.resetBytes(in: 0..<data.count) }
            var attributes = try query()
            let changes: [String: Any] = [kSecValueData as String: data,
                kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlocked]
            let updated = SecItemUpdate(attributes as CFDictionary, changes as CFDictionary)
            if updated == errSecItemNotFound {
                attributes.merge(changes) { _, new in new }
                try check(SecItemAdd(attributes as CFDictionary, nil))
            } else { try check(updated) }
        }
    }
    static func restore() async throws {
        try await BlockingWork.run(on: queue) {
            var attributes = try query()
            attributes[kSecReturnData as String] = true
            attributes[kSecMatchLimit as String] = kSecMatchLimitOne
            var result: CFTypeRef?
            let status = SecItemCopyMatching(attributes as CFDictionary, &result)
            if status == errSecItemNotFound {
                throw SharedLoginFailure(message: "No shared login has arrived. Share it from Wendy on your other device, enable iCloud Passwords & Keychain on both devices, and try again after synchronization.")
            }
            try check(status)
            guard var data = result as? Data, !data.isEmpty, data.count <= 65_536 else {
                throw SharedLoginFailure(message: "The shared login is invalid or too large.")
            }
            defer { data.resetBytes(in: 0..<data.count) }
            let count = data.count
            try data.withUnsafeMutableBytes { bytes in
                guard let result = WDSharedLoginImport(bytes.bindMemory(to: UInt8.self).baseAddress, count) else {
                    throw SharedLoginFailure(message: "Could not use the shared login.")
                }
                defer { WDStatusFree(result) }
                if let text = result.pointee.message { throw SharedLoginFailure(message: String(cString: text)) }
            }
        }
    }
    static func remove() async throws {
        try await BlockingWork.run(on: queue) {
            let status = SecItemDelete(try query() as CFDictionary)
            if status != errSecItemNotFound { try check(status) }
        }
    }
}

struct SharedLoginView: View {
    var onImport: @MainActor () async -> Void = {}
    @State private var actions = AsyncActions()
    @State private var busy = false
    @State private var message: String?
    @State private var failed = false
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Label("iCloud Keychain", systemImage: "key.icloud").font(.headline)
            Text("Share your current Wendy login with your other Apple devices. Both apps need the same Apple Account with Passwords & Keychain enabled.")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                Button("Share login") { run {
                    try await SharedLoginKeychain.publish()
                    return "Login stored for iCloud synchronization. On the other device, choose Use shared login."
                } }
                Button("Use shared login") { run {
                    try await SharedLoginKeychain.restore()
                    await onImport()
                    return "Shared login is active on this device."
                } }
                Menu {
                    Button("Remove shared login", role: .destructive) { run {
                        try await SharedLoginKeychain.remove()
                        return "Shared Keychain entry removed. Existing imported sessions remain valid until they expire or are revoked."
                    } }
                } label: { Image(systemName: "ellipsis") }.accessibilityLabel("Sharing options")
                if busy { ProgressView().controlSize(.small) }
            }.disabled(busy)
            Text("Use the shared copy within 24 hours, before its credentials expire. Imported credentials retain their original expiry. Share again when needed; refresh tokens and device-specific registry keys stay on their original device.")
                .font(.caption2).foregroundStyle(.secondary)
            if let message { Text(message).font(.caption).foregroundStyle(failed ? .orange : .secondary).textSelection(.enabled) }
        }.task { await actions.run() }
    }
    private func run(_ operation: @escaping @MainActor () async throws -> String) {
        guard !busy else { return }
        busy = true; message = nil; failed = false
        actions.submit {
            defer { busy = false }
            do { message = try await operation() }
            catch { failed = true; message = error.localizedDescription }
        }
    }
}
