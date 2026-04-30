public enum OnceError: Error {
    case failedOnFirstRun(originalError: Error)
}

// MARK: - CustomStringConvertible

extension OnceError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .failedOnFirstRun(let originalError):
            return "Once failed on first run: \(originalError)"
        }
    }
}
