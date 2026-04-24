public struct WendyAgentConfiguration: Sendable {
    public var port: Int
    public var otelPort: Int
    public var appPath: String
    public var sandboxProfile: String

    public init(
        port: Int = 50051,
        otelPort: Int = 4317,
        appPath: String = "",
        sandboxProfile: String = ""
    ) {
        self.port = port
        self.otelPort = otelPort
        self.appPath = appPath
        self.sandboxProfile = sandboxProfile
    }
}
