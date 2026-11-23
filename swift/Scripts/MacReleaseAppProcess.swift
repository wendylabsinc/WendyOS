import AppKit
import Darwin
import Foundation

// LaunchServices can resolve a bundle ID to a different installed copy. Release
// validation must observe and quit only the exact bundle being packaged.
guard CommandLine.arguments.count == 3 else { exit(64) }
let action = CommandLine.arguments[1]
let expected = URL(fileURLWithPath: CommandLine.arguments[2], isDirectory: true)
    .resolvingSymlinksInPath().standardizedFileURL
let applications = NSRunningApplication.runningApplications(
    withBundleIdentifier: "sh.wendy.WendyAgentMac"
).filter { !$0.isTerminated }
let owned = applications.filter {
    $0.bundleURL?.resolvingSymlinksInPath().standardizedFileURL == expected
}

switch action {
case "running":
    exit(owned.isEmpty ? 1 : 0)
case "exclusive":
    exit(applications.count == owned.count ? 0 : 1)
case "quit":
    for app in owned { _ = app.terminate() }
case "diagnose":
    for app in applications {
        print(
            "pid=\(app.processIdentifier) bundle=\(app.bundleURL?.path ?? "unknown") executable=\(app.executableURL?.path ?? "unknown")"
        )
    }
default:
    exit(64)
}
