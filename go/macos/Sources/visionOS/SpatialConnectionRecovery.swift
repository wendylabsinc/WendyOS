import Foundation

/// EOF alone is not proof of missing credentials. Offer authentication recovery
/// only when this app also has no usable account, retaining the original error.
enum SpatialConnectionRecovery {
    static func needsLogin(signedIn: Bool, error: String) -> Bool {
        guard !signedIn else { return false }
        let message = error.lowercased()
        return ["error reading server preface: eof", "unauthenticated", "certificate required",
                "not logged in", "wendy auth login"].contains { message.contains($0) }
    }
}
