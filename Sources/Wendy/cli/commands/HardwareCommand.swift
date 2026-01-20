import ArgumentParser
import Foundation
import Logging

struct HardwareCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "hardware",
        abstract: "Discover and list hardware capabilities on the wendy device"
    )

    @Option(
        help:
            "Filter by hardware category (gpu, usb, i2c, spi, gpio, camera, audio, input, serial, network, storage)"
    )
    var category: String?

    @OptionGroup var agentConnectionOptions: AgentConnectionOptions

    func run() async throws {
        let logger = Logger(label: "hardware.discovery")

        do {
            let capabilities = try await withAgentClient(
                agentConnectionOptions,
                title: "For which device do you want to discover hardware?"
            ) { client in
                try await client.listHardware(categoryFilter: category)
            }

            if JSONMode.isEnabled {
                try outputJSON(capabilities)
            } else {
                outputText(capabilities, logger: logger)
            }
        } catch {
            logger.error("Failed to discover hardware", metadata: ["error": "\(error)"])
            throw ExitCode.failure
        }
    }

    private func outputJSON(_ capabilities: [HardwareCapability]) throws {
        let jsonCapabilities = capabilities.map { capability in
            return [
                "category": capability.category,
                "devicePath": capability.devicePath,
                "description": capability.description,
                "properties": capability.properties,
            ] as [String: Any]
        }

        let jsonData = try JSONSerialization.data(
            withJSONObject: jsonCapabilities,
            options: [.prettyPrinted, .sortedKeys]
        )
        let jsonString = String(data: jsonData, encoding: .utf8) ?? "[]"
        print(jsonString)
    }

    private func outputText(_ capabilities: [HardwareCapability], logger: Logger) {
        if capabilities.isEmpty {
            if let categoryFilter = category {
                print("No \(categoryFilter) hardware found on this device.")
            } else {
                print("No hardware capabilities discovered on this device.")
            }
            return
        }

        // Group capabilities by category
        let groupedCapabilities = Dictionary(grouping: capabilities, by: { $0.category })
        let sortedCategories = groupedCapabilities.keys.sorted()

        print("Hardware Capabilities:")
        print("===================")
        print()

        for category in sortedCategories {
            let categoryCapabilities = groupedCapabilities[category]!
            print(
                "📁 \(category.uppercased()) (\(categoryCapabilities.count) device\(categoryCapabilities.count == 1 ? "" : "s"))"
            )

            for capability in categoryCapabilities.sorted(by: { $0.devicePath < $1.devicePath }) {
                print("  🔧 \(capability.devicePath)")
                print("     Description: \(capability.description)")

                if !capability.properties.isEmpty {
                    print("     Properties:")
                    let sortedProperties = capability.properties.sorted { $0.key < $1.key }
                    for (key, value) in sortedProperties {
                        print("       • \(key): \(value)")
                    }
                }
                print()
            }
        }

        // Summary
        let totalDevices = capabilities.count
        let categoryCount = groupedCapabilities.keys.count
        print(
            "Summary: \(totalDevices) hardware device\(totalDevices == 1 ? "" : "s") across \(categoryCount) categor\(categoryCount == 1 ? "y" : "ies")"
        )

        if self.category == nil {
            print("\nTip: Use --category <type> to filter by specific hardware type")
            print(
                "Available categories: audio, camera, gpu, gpio, i2c, input, network, serial, spi, storage, usb"
            )
        }
    }
}
