public struct WendyAppInfo: Sendable, Equatable, Codable {
    public enum Kind: String, Sendable, Equatable, Codable {
        case native
        case container
    }

    public enum Status: String, Sendable, Equatable, Codable {
        case stopped
        case running
    }

    public let id: String
    public let kind: Kind
    public let status: Status
    public let pid: Int32?

    public init(id: String, kind: Kind, status: Status, pid: Int32?) {
        self.id = id
        self.kind = kind
        self.status = status
        self.pid = pid
    }
}
