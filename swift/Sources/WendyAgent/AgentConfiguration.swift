public struct AgentConfiguration: Sendable {
    public var port: Int
    public var otelPort: Int
    public var configDirectory: String
    public var appPath: String
    public var sandboxProfile: String

    public init(
        port: Int = 50051,
        otelPort: Int = 4317,
        configDirectory: String = "/etc/wendy-agent",
        appPath: String = "",
        sandboxProfile: String = ""
    ) {
        self.port = port
        self.otelPort = otelPort
        self.configDirectory = configDirectory
        self.appPath = appPath
        self.sandboxProfile = sandboxProfile
    }
}
