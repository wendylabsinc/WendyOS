import ArgumentParser
import Foundation
import Testing

@testable import wendy

@Suite("Hardware Command Tests")
struct HardwareCommandTests {

    @Test("HardwareCommand basic configuration")
    func testHardwareCommandConfiguration() async throws {
        // Test that the command configuration is set up correctly
        let config = HardwareCommand.configuration
        #expect(config.commandName == "hardware")
        #expect(config.abstract == "Discover and list hardware capabilities on the wendy device")
    }

    @Test("HardwareCommand argument parsing with category")
    func testHardwareCommandArgumentParsing() async throws {
        // Test parsing arguments for the hardware command
        do {
            let command =
                try HardwareCommand.parseAsRoot(["--category", "gpu"]) as! HardwareCommand
            #expect(command.category == "gpu")
        } catch {
            #expect(Bool(false), "Failed to parse valid arguments: \(error)")
        }
    }

    @Test("HardwareCommand default values")
    func testHardwareCommandDefaults() async throws {
        // Test default values
        let command = try HardwareCommand.parseAsRoot([]) as! HardwareCommand
        #expect(command.category == nil)
    }

    @Test("HardwareCommand invalid category handling")
    func testHardwareCommandInvalidUsage() async throws {
        // Test that the command can be constructed with any category string
        // (validation happens on the server side)
        let command =
            try HardwareCommand.parseAsRoot(["--category", "invalid_category"]) as! HardwareCommand
        #expect(command.category == "invalid_category")
    }

    @Test("JSONMode TaskLocal default is false")
    func testJSONModeDefault() async throws {
        // Test that JSON mode is disabled by default
        #expect(JSONMode.isEnabled == false)
    }

    @Test("JSONMode can be enabled via withJSONMode")
    func testJSONModeEnabled() async throws {
        // Test that JSON mode can be enabled
        await withJSONMode(enabled: true) {
            #expect(JSONMode.isEnabled == true)
        }
        // After the closure, it should be back to false
        #expect(JSONMode.isEnabled == false)
    }
}
