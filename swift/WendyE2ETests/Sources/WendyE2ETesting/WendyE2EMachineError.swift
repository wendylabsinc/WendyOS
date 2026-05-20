public enum WendyE2EMachineError: Error {
    case commandFailed(machine: String, command: String, status: WendyE2EShellStatus)
    case powerShellUnavailable(machine: String)
}

// MARK: - CustomStringConvertible

extension WendyE2EMachineError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .commandFailed(let machine, let command, let status):
            return "Command failed on \(machine) with \(status): \(command)"
        case .powerShellUnavailable(let machine):
            return "PowerShell is not available on \(machine)"
        }
    }
}
