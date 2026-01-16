import ArgumentParser
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import WendyAgentGRPC
import WendyShared

#if canImport(Bluetooth)
    import Bluetooth
#endif

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
            let capabilities = try await discoverHardware()

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

    private func discoverHardware() async throws
        -> [Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse.HardwareCapability]
    {
        return try await withAgentConnection(
            agentConnectionOptions,
            title: "For which device do you want to discover hardware?",
            grpcOperation: { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)

                var request = Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest()
                if let categoryFilter = self.category {
                    request.categoryFilter = categoryFilter
                }

                let response = try await agent.listHardwareCapabilities(request)
                return response.capabilities
            },
            bluetoothOperation: { deviceIdentifier in
                #if canImport(Bluetooth)
                    let response = try await executeBluetoothCommand(
                        .hardwareList,
                        deviceIdentifier: deviceIdentifier
                    )
                    if case .hardwareList(let capabilities) = response {
                        // Convert BluetoothHardwareInfo to gRPC HardwareCapability
                        return capabilities.map { info in
                            var capability =
                                Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse
                                .HardwareCapability()
                            capability.category = info.type
                            capability.description_p = info.name
                            return capability
                        }
                    } else if case .error(let message) = response {
                        throw HardwareCommandError.operationFailed(message)
                    }
                    return []
                #else
                    throw BluetoothNotAvailableError()
                #endif
            }
        )
    }

    enum HardwareCommandError: Error, LocalizedError {
        case operationFailed(String)

        var errorDescription: String? {
            switch self {
            case .operationFailed(let message):
                return message
            }
        }
    }

    private func outputJSON(
        _ capabilities: [Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse
            .HardwareCapability]
    ) throws {
        let jsonCapabilities = capabilities.map { capability in
            return [
                "category": capability.category,
                "devicePath": capability.devicePath,
                "description": capability.description_p,
                "properties": capability.properties,
            ]
        }

        let jsonData = try JSONSerialization.data(
            withJSONObject: jsonCapabilities,
            options: [.prettyPrinted, .sortedKeys]
        )
        let jsonString = String(data: jsonData, encoding: .utf8) ?? "[]"
        print(jsonString)
    }

    private func outputText(
        _ capabilities: [Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse
            .HardwareCapability],
        logger: Logger
    ) {
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
                print("     Description: \(capability.description_p)")

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

        if category == nil {
            print("\nTip: Use --category <type> to filter by specific hardware type")
            print(
                "Available categories: audio, camera, gpu, gpio, i2c, input, network, serial, spi, storage, usb"
            )
        }
    }
}
