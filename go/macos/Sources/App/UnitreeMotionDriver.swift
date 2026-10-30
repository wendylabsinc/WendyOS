#if os(macOS)
import SwiftUI

// One sender handles the current velocity, key release and focus loss. Input
// changes update desired motion; they never create competing network tasks.
@MainActor
final class UnitreeMotionDriver: ObservableObject {
    @Published var enabled = false
    @Published private(set) var direction: String?
    @Published var error: String?
    private(set) var desired = UnitreeMotion()
    private var stopRequested = false
    private var nextSend = Date.distantPast
    private let send: (UnitreeMotion) async throws -> Void

    init(send: @escaping (UnitreeMotion) async throws -> Void) { self.send = send }
    convenience init(handle: UInt64) {
        self.init { motion in
            try await UnitreeClient.shared.move(handle: handle, forward: motion.forward, lateral: motion.lateral, yaw: motion.yaw)
        }
    }

    func update(_ motion: UnitreeMotion, direction: String) {
        guard enabled else { return }
        desired = motion
        self.direction = motion == UnitreeMotion() ? nil : direction
        if motion == UnitreeMotion() { stopRequested = true }
    }

    func stop(disable: Bool = false) {
        if disable { enabled = false }
        desired = UnitreeMotion(); direction = nil; stopRequested = true
    }

    func step(now: Date = Date()) async {
        let stopping = stopRequested
        let motion = stopping ? UnitreeMotion() : desired
        guard stopping || (enabled && motion != UnitreeMotion() && now >= nextSend) else { return }
        stopRequested = false
        nextSend = now.addingTimeInterval(0.1)
        do { try await send(motion) }
        catch {
            enabled = false; desired = UnitreeMotion(); direction = nil
            self.error = stopping ? "Stop could not be confirmed. Use the physical remote." : error.localizedDescription
            // A failed movement still needs a Stop attempt. A failed Stop is
            // reported once, rather than filling the connection with retries.
            if !stopping { stopRequested = true }
        }
    }

    func run() async {
        while !Task.isCancelled {
            await step()
            do { try await Task.sleep(for: .milliseconds(20)) }
            catch { break }
        }
        desired = UnitreeMotion(); direction = nil
        try? await send(UnitreeMotion())
    }
}
#endif
