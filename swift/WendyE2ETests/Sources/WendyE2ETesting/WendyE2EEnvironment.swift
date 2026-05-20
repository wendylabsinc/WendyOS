import Foundation

public enum WendyE2EIsolation: String, Sendable {
    case none
    case perRun = "per-run"
    case perTest = "per-test"

    public init?(environmentValue: String) {
        self.init(rawValue: environmentValue.lowercased())
    }
}

public enum WendyE2EEnvironment {
    public static let runID: String = {
        let configured = value("WENDY_E2E_RUN_ID") ?? UUID().uuidString
        return configured.replacingOccurrences(of: "-", with: "")
    }()

    public static var verbose: Bool {
        flag("WENDY_E2E_VERBOSE")
    }

    public static var parallel: Bool {
        flag("WENDY_E2E_PARALLEL")
    }

    public static var runDirectory: String? {
        value("WENDY_E2E_RUN_DIR")
    }

    public static var isolation: WendyE2EIsolation {
        value("WENDY_E2E_ISOLATION").flatMap(WendyE2EIsolation.init(environmentValue:)) ?? .perTest
    }

    public static var cliOS: WendyE2EMachineOS? {
        value("WENDY_E2E_CLI_OS").flatMap(WendyE2EMachineOS.init(environmentValue:))
    }

    public static var cliUser: String? {
        value("WENDY_E2E_CLI_USER")
    }

    public static var cliAddress: String? {
        value("WENDY_E2E_CLI_ADDRESS")
    }

    public static var cliRunDirectory: String? {
        value("WENDY_E2E_CLI_RUN_DIR")
    }

    public static var cliRepoDirectory: String? {
        value("WENDY_E2E_CLI_REPO_DIR")
    }

    public static var cliBinDirectory: String? {
        value("WENDY_E2E_CLI_BIN_DIR")
    }

    public static var agentOS: WendyE2EMachineOS? {
        value("WENDY_E2E_AGENT_OS").flatMap(WendyE2EMachineOS.init(environmentValue:))
    }

    public static var agentUser: String? {
        value("WENDY_E2E_AGENT_USER")
    }

    public static var agentAddress: String? {
        value("WENDY_E2E_AGENT_ADDRESS")
    }

    public static var agentRunDirectory: String? {
        value("WENDY_E2E_AGENT_RUN_DIR")
    }

    public static var agentRepoDirectory: String? {
        value("WENDY_E2E_AGENT_REPO_DIR")
    }

    public static var agentBinDirectory: String? {
        value("WENDY_E2E_AGENT_BIN_DIR")
    }

    public static var testRecordsDirectory: String? {
        value("WENDY_E2E_RECORDING_DIR") ?? value("WENDY_E2E_TEST_RECORDS_DIR")
    }

    private static func value(_ name: String) -> String? {
        guard let value = ProcessInfo.processInfo.environment[name], !value.isEmpty else {
            return nil
        }
        return value
    }

    private static func flag(_ name: String) -> Bool {
        guard let value = value(name)?.lowercased() else {
            return false
        }
        return ["1", "true", "yes", "on", "enabled"].contains(value)
    }
}
