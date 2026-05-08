public import Foundation

public enum WendyAgentError: CustomStringConvertible, LocalizedError, Sendable {
    case stoppedDuringStartup
    case portInUse(serviceName: String, port: Int)

    public var description: String {
        self.errorDescription ?? "WendyAgent error."
    }

    public var errorDescription: String? {
        switch self {
        case .stoppedDuringStartup:
            "WendyAgent stopped before startup completed."
        case .portInUse(let serviceName, let port):
            """
            Could not start the \(serviceName) server because TCP port \(port) is already in use. Another WendyAgentMac may already be running for another macOS user. Quit the other WendyAgentMac or free port \(port), then try again.
            """
        }
    }
}
