import Darwin
import Foundation

protocol AgentRelaunchScheduling: Sendable {
    /// Spawns the detached watcher that reopens the bundle after this process exits.
    func scheduleRelaunch(of bundleURL: URL) throws
    /// Kicks off process termination after a short ack-flush delay.
    func scheduleTermination()
}

/// Spawns a detached watcher process that waits for this agent process to
/// exit, then reopens the (freshly swapped-in) app bundle — the mechanism
/// that turns "install a new bundle" into "the new version is running."
struct AgentRelauncher: AgentRelaunchScheduling {
    /// Injected by the app target (clean quit path); nil ⇒ exit(0).
    let terminate: (@Sendable () async -> Void)?

    /// The app target's update-specific termination path does not stop deployed
    /// apps; they survive the agent process and are adopted after relaunch. This
    /// is only a watchdog for AppKit failing to terminate promptly.
    static let hardExitDelay: Duration = .seconds(10)

    /// How long the gRPC ack gets to flush to the client before the graceful
    /// quit tears the connection down.
    static let ackFlushDelay: Duration = .milliseconds(500)

    func scheduleRelaunch(of bundleURL: URL) throws {
        let pid = ProcessInfo.processInfo.processIdentifier
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sh")
        process.arguments = Self.makeArguments(pid: pid, bundlePath: bundleURL.path)
        // Null all three stdio handles so the child does not hold our pipes
        // open — it must be able to outlive us cleanly.
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice

        try process.run()
        // Deliberately not waiting: once this process exits, the child is
        // reparented and survives — it IS the mechanism that relaunches the
        // app after we're gone.
    }

    /// Builds the `/bin/sh -c <script> wendy-agent-relaunch <pid> <bundlePath>`
    /// arguments. `pid`/`bundlePath` are passed as positional args (`$1`/`$2`)
    /// rather than interpolated into the script text, avoiding shell quoting
    /// bugs for paths with spaces or other special characters.
    static func makeArguments(pid: Int32, bundlePath: String) -> [String] {
        let script = """
            i=0
            while /bin/kill -0 "$1" 2>/dev/null; do
              /bin/sleep 0.5
              i=$((i+1)); [ "$i" -ge 600 ] && exit 1
            done
            exec /usr/bin/open -n "$2"
            """
        return ["-c", script, "wendy-agent-relaunch", String(pid), bundlePath]
    }

    func scheduleTermination() {
        // Hard watchdog: if a hung graceful stop never calls `terminate`,
        // force the process down anyway so the update can't wedge forever.
        Task.detached {
            try? await Task.sleep(for: Self.hardExitDelay)
            exit(0)
        }
        // Give the gRPC ack a brief moment to flush to the client before we
        // tear the connection down.
        Task.detached {
            try? await Task.sleep(for: Self.ackFlushDelay)
            if let terminate = self.terminate {
                await terminate()
            } else {
                exit(0)
            }
        }
    }
}
