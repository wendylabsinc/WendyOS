import Foundation

/// A short-lived, process-bound handle to an app identity broker.
///
/// The socket path is not a credential. A provider owns the endpoint and must
/// withhold all identity material until `activateIdentity` binds this lease to
/// the process Wendy actually launched.
struct NativeAppIdentityLease: Equatable, Sendable {
    static let environmentKey = "WENDY_APP_IDENTITY_SOCKET"

    let id: UUID
    let socketPath: String

    init(socketPath: String) throws {
        let standardizedPath = URL(fileURLWithPath: socketPath).standardizedFileURL.path
        guard socketPath.hasPrefix("/"), socketPath != "/", socketPath == standardizedPath,
            socketPath.utf8.count <= 103,
            !socketPath.unicodeScalars.contains(where: {
                CharacterSet.controlCharacters.contains($0)
            })
        else {
            throw NativeAppIdentityError.invalidSocketPath
        }

        self.id = UUID()
        self.socketPath = socketPath
    }
}

enum NativeAppIdentityError: Error, Equatable {
    case invalidSocketPath
}

/// Pluggable boundary for app-scoped identity without placing credentials in
/// app configuration, persisted launch metadata, command arguments, or the
/// process environment.
///
/// Implementations must create a fresh private local socket in `prepareIdentity`,
/// serve no identity-bearing operation until `activateIdentity` binds the lease
/// to the expected pid, and destroy all lease state in `revokeIdentity`. The
/// default provider deliberately issues nothing until a Cloud-backed issuer and
/// broker are available.
protocol NativeAppIdentityProviding: Sendable {
    func prepareIdentity(forAppID appID: String) async throws -> NativeAppIdentityLease?
    func activateIdentity(_ lease: NativeAppIdentityLease, forProcessID pid: Int32) async throws
    func revokeIdentity(_ lease: NativeAppIdentityLease) async
}

struct UnavailableNativeAppIdentityProvider: NativeAppIdentityProviding {
    func prepareIdentity(forAppID appID: String) async throws -> NativeAppIdentityLease? {
        nil
    }

    func activateIdentity(_ lease: NativeAppIdentityLease, forProcessID pid: Int32) async throws {}

    func revokeIdentity(_ lease: NativeAppIdentityLease) async {}
}
