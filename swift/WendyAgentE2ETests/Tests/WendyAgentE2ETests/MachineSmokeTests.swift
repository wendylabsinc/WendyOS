import Foundation
import Testing

@testable import WendyAgentE2E

extension Tag {
    @Tag static var e2e: Self
}

@Suite("Machine smoke tests", .serialized, .tags(.e2e))
struct MachineSmokeTests {
    @Test("build over SSH machine", .timeLimit(.minutes(10)))
    func buildOverSSHMachine() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard environment["WENDY_E2E_SMOKE"] == "1" else {
            return
        }

        let ssh = try #require(environment["E2E_MACHINE_SSH"])
        let path = try #require(environment["E2E_MACHINE_PATH"])
        let machine = try Machine(ssh: ssh, path: path)

        try await machine.run("cd swift && make build-dev")
        try await machine.run("cd go && make build")
    }
}
