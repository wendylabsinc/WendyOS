import Darwin
import Foundation

// Compiled beside the unchanged production AgentRelauncher, without AppKit,
// an app bundle, launchd registration, user preferences or installed services.
@main
struct AgentTerminationProcess {
    static func record(_ name: String, in directory: URL, start: TimeInterval) {
        let elapsed = ProcessInfo.processInfo.systemUptime - start
        do {
            try Data(String(elapsed).utf8).write(
                to: directory.appendingPathComponent(name),
                options: .atomic
            )
        } catch {
            exit(91)
        }
    }

    static func main() async {
        guard CommandLine.arguments.count == 3 else { exit(92) }
        let mode = CommandLine.arguments[1]
        let directory = URL(fileURLWithPath: CommandLine.arguments[2])
        let start = ProcessInfo.processInfo.systemUptime
        let terminate: (@Sendable () async -> Void)?
        switch mode {
        case "graceful", "hung":
            terminate = {
                record("callback", in: directory, start: start)
                if mode == "graceful" { exit(0) }
                // Model the original failure: graceful termination never exits.
                // This deliberately outlasts both current and historical timers.
                try? await Task.sleep(for: .seconds(90))
                exit(93)
            }
        case "nil":
            terminate = nil
        default:
            exit(94)
        }
        AgentRelauncher(terminate: terminate).scheduleTermination()
        record("armed", in: directory, start: start)
        try? await Task.sleep(for: .milliseconds(200))
        // A real post-scheduling write must survive the production ack delay.
        record("ack", in: directory, start: start)
        try? await Task.sleep(for: .seconds(90))
        exit(95)
    }
}
