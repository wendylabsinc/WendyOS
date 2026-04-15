import Foundation

public enum WendyAgentError: LocalizedError, Sendable {
    case stoppedDuringStartup

    public var errorDescription: String? {
        switch self {
        case .stoppedDuringStartup:
            "WendyAgent stopped before startup completed."
        }
    }
}
