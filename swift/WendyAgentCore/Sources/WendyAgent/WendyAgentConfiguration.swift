public struct WendyAgentConfiguration: Sendable {
    public var host: String
    public var port: Int
    public var otelPort: Int
    public var appPath: String
    public var sandboxProfile: String

    public init(
        host: String = "127.0.0.1",
        port: Int = 50051,
        otelPort: Int = 4317,
        appPath: String = "",
        sandboxProfile: String = ""
    ) {
        self.host = host
        self.port = port
        self.otelPort = otelPort
        self.appPath = appPath
        self.sandboxProfile = sandboxProfile
    }

    /// Remote listeners, discovery and cloud relays must share the same policy.
    public var allowsNetworkAccess: Bool {
        !["127.0.0.1", "::1", "localhost"].contains(self.host)
    }
}

extension WendyAgentConfiguration {
    public static var `default`: Self {
        var configuration = Self()

        if let host = WendyAgentEnvironment.host {
            configuration.host = host
        }
        if let port = WendyAgentEnvironment.port {
            configuration.port = port
        }
        if let otelPort = WendyAgentEnvironment.otelPort {
            configuration.otelPort = otelPort
        }
        if let appPath = WendyAgentEnvironment.appPath {
            configuration.appPath = appPath
        }
        if let sandboxProfile = WendyAgentEnvironment.sandboxProfile {
            configuration.sandboxProfile = sandboxProfile
        }

        return configuration
    }
}
