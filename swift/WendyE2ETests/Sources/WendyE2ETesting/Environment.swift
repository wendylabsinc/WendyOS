import Foundation

public enum Environment {
    public static let runID: String = {
        let configured = value("WENDY_E2E_RUN_ID") ?? UUID().uuidString
        return configured.replacingOccurrences(of: "-", with: "")
    }()

    public static var verbose: Bool {
        flag("WENDY_E2E_VERBOSE")
    }

    public static var cliOS: MachineOS? {
        value("WENDY_E2E_CLI_OS").flatMap(MachineOS.init(environmentValue:))
    }

    public static var cliUser: String? {
        value("WENDY_E2E_CLI_USER")
    }

    public static var cliAddress: String? {
        value("WENDY_E2E_CLI_ADDRESS")
    }

    public static var cliWorkingDirectory: String? {
        value("WENDY_E2E_CLI_WORKING_DIRECTORY")
    }

    public static var agentOS: MachineOS? {
        value("WENDY_E2E_AGENT_OS").flatMap(MachineOS.init(environmentValue:))
    }

    public static var agentUser: String? {
        value("WENDY_E2E_AGENT_USER")
    }

    public static var agentAddress: String? {
        value("WENDY_E2E_AGENT_ADDRESS")
    }

    public static var agentWorkingDirectory: String? {
        value("WENDY_E2E_AGENT_WORKING_DIRECTORY")
    }

    public static var testRecordsDirectory: String? {
        value("WENDY_E2E_TEST_RECORDS_DIR")
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
        return ["1", "true", "yes", "on"].contains(value)
    }
}
