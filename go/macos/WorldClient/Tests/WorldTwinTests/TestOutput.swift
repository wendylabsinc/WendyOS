import Foundation

enum TestOutput {
    static func directory(_ name: String) -> URL {
        let base = ProcessInfo.processInfo.environment["WENDY_WORLD_REVIEW_OUTPUT"].map {
            URL(fileURLWithPath: $0, isDirectory: true)
        } ?? FileManager.default.temporaryDirectory.appendingPathComponent("WendyWorldTwinChecks", isDirectory: true)
        return base.appendingPathComponent(name + "-" + UUID().uuidString, isDirectory: true)
    }
}
