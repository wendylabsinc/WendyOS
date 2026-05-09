import Foundation

public enum Environment {
    public static var verbose: Bool {
        flag("WENDY_E2E_VERBOSE")
    }

    public static var cliOS: MachineOS? {
        value("WENDY_E2E_CLI_OS").flatMap(MachineOS.init(environmentValue:))
    }

    public static var cliSSH: String? {
        value("WENDY_E2E_CLI_SSH")
    }

    public static var cliWorkingDirectory: String? {
        value("WENDY_E2E_CLI_WORKING_DIRECTORY")
    }

    public static var agentOS: MachineOS? {
        value("WENDY_E2E_AGENT_OS").flatMap(MachineOS.init(environmentValue:))
    }

    public static var agentSSH: String? {
        value("WENDY_E2E_AGENT_SSH")
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
