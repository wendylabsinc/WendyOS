public enum WendyAgentStatus: Equatable, Sendable {
    case idle
    case starting
    case running
    case stopping
    case stopped
    case failed(String)
}
