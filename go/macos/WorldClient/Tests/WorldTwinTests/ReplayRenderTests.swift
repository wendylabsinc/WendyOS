import AppKit
import SwiftUI
import XCTest
@testable import WorldTwin

final class ReplayRenderTests: XCTestCase {
    @MainActor
    func testRetainedSessionRendersWithoutWritingToRun() async throws {
        guard let input = ProcessInfo.processInfo.environment["WENDY_WORLD_REPLAY_INPUT"] else {
            throw XCTSkip("Supply a retained simulation viewer directory for native replay rendering.")
        }
        let directory = URL(fileURLWithPath: input, isDirectory: true)
        let before = try files(in: directory)
        let launch = try XCTUnwrap(WorldTwinLaunchRequest.parse(["--world-session", input, "--world-replay"]))
        let host = NSHostingView(rootView: WorldTwinWorkspace(launch: launch))
        let bounds = NSRect(x: 0, y: 0, width: 1680, height: 1050)
        let window = NSWindow(contentRect: bounds, styleMask: [.titled], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.contentView = host
        host.frame = bounds
        host.layoutSubtreeIfNeeded()
        defer { window.contentView = nil; window.close() }
        try await Task.sleep(for: .milliseconds(800))
        host.layoutSubtreeIfNeeded()
        host.displayIfNeeded()
        let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: bitmap)
        let pixels = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
        XCTAssertGreaterThan(pixels.count, 10_000)
        let output = TestOutput.directory("replay-render")
        try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
        try pixels.write(to: output.appendingPathComponent("integrated-world-replay.png"))
        XCTAssertEqual(try files(in: directory), before, "Replay must not write commands, receipts, capture requests or stop signals into the original run.")
    }

    private func files(in directory: URL) throws -> [String: Date] {
        let enumerator = try XCTUnwrap(FileManager.default.enumerator(at: directory, includingPropertiesForKeys: [.contentModificationDateKey, .isRegularFileKey]))
        var result: [String: Date] = [:]
        for case let url as URL in enumerator {
            let value = try url.resourceValues(forKeys: [.contentModificationDateKey, .isRegularFileKey])
            if value.isRegularFile == true { result[url.path] = value.contentModificationDate ?? .distantPast }
        }
        return result
    }
}
