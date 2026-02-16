import ArgumentParser
import Foundation
import WendyShared

struct UpdateCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "update",
        abstract: "Check for CLI updates"
    )

    func run() async throws {
        // Force check, ignoring cooldowns
        await UpdateChecker.forceCheck()
    }
}
