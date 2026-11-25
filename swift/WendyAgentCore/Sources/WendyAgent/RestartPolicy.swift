import WendyAgentGRPC

/// The restart policy persisted alongside a `WendyApp`, independent of the
/// wire representation (`Wendy_Agent_Services_V1_RestartPolicy`/`Wendy_Agent_Services_V1_RestartPolicyMode` from
/// `shared.proto`). Kept separate from the generated proto type so the
/// on-disk shape doesn't change if the wire format does.
struct PersistedRestartPolicy: Codable, Equatable, Sendable {
    enum Mode: String, Codable, Equatable, Sendable {
        case unlessStopped
        case no
        case onFailure
    }

    var mode: Mode
    var onFailureMaxRetries: Int

    static let `default` = PersistedRestartPolicy(mode: .unlessStopped, onFailureMaxRetries: 0)

    init(mode: Mode, onFailureMaxRetries: Int) {
        self.mode = mode
        self.onFailureMaxRetries = onFailureMaxRetries
    }

    /// Maps from the generated proto type. `DEFAULT` (the field's zero value,
    /// meaning "the request didn't set one") and any future/unrecognized mode
    /// both fall back to unless-stopped, matching the agent's default.
    init(from proto: Wendy_Agent_Services_V1_RestartPolicy) {
        switch proto.mode {
        case .default, .unlessStopped:
            self.mode = .unlessStopped
            self.onFailureMaxRetries = 0
        case .no:
            self.mode = .no
            self.onFailureMaxRetries = 0
        case .onFailure:
            self.mode = .onFailure
            self.onFailureMaxRetries = Int(proto.onFailureMaxRetries)
        case .UNRECOGNIZED:
            self.mode = .unlessStopped
            self.onFailureMaxRetries = 0
        }
    }
}
