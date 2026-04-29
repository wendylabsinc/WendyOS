import Subprocess

// MARK: - Public

public enum MachineError: Error {
    case invalidMachineSpec(String)
    case connectionFailed(machine: String, stderr: String)
    case commandFailed(machine: String, command: String, terminationStatus: TerminationStatus)
}

// MARK: - CustomStringConvertible

extension MachineError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .invalidMachineSpec(let spec):
            return "Invalid machine spec: \(spec)"
        case .connectionFailed(let machine, let stderr):
            if stderr.isEmpty {
                return "Failed to establish SSH session for \(machine)"
            }
            return "Failed to establish SSH session for \(machine):\n\(stderr)"
        case .commandFailed(let machine, let command, let terminationStatus):
            return "Command failed on \(machine) with \(terminationStatus): \(command)"
        }
    }
}
