import ArgumentParser
import AsyncAlgorithms
import Foundation
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

struct DiscoverCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "discover",
        abstract: "Find connected Wendy devices"
    )

    enum DeviceType: String, ExpressibleByArgument {
        case usb, ethernet, lan, all
    }

    @Option(help: "Device types to list (usb, ethernet, lan, or all)")
    var type: DeviceType = .all

    @Flag(help: "Skip resolving the agent's version")
    var skipResolveAgentVersion: Bool = false

    private func discoverDevices() async throws -> DevicesCollection {
        let logger = Logger(label: "sh.wendy.cli.devices")
        let discovery = PlatformDeviceDiscovery(logger: logger)
        // Collect devices based on the requested type
        var usbDevices: [USBDevice] = []
        var ethernetDevices: [EthernetInterface] = []
        var lanDevices: [LANDevice] = []

        switch type {
        case .usb:
            usbDevices = await discovery.findUSBDevices()
        case .ethernet:
            ethernetDevices = await discovery.findEthernetInterfaces()
        case .lan:
            lanDevices = try await discovery.findLANDevices()
        case .all:
            // Fetch all types of devices
            async let _usbDevices = await discovery.findUSBDevices()
            async let _ethernetDevices = await discovery.findEthernetInterfaces()
            async let _lanDevices = try await discovery.findLANDevices()

            usbDevices = await _usbDevices
            ethernetDevices = await _ethernetDevices
            lanDevices = try await _lanDevices
        }

        // Display devices in the requested format
        var collection = DevicesCollection(
            usb: usbDevices,
            ethernet: ethernetDevices,
            lan: lanDevices
        )

        if !skipResolveAgentVersion {
            collection = try await collection.resolveAgentVersions()
        }

        return collection
    }

    func run() async throws {
        let logger = Logger(label: "sh.wendy.cli.devices")

        if JSONMode.isEnabled {
            let collection = try await discoverDevices()
            do {
                let jsonOutput = try collection.toJSON()
                print(jsonOutput)
            } catch {
                logger.error("Error serializing to JSON: \(error)")
            }
        } else {
            let collection = try await Noora().progressStep(message: "Discovering Wendy devices") {
                progress in
                try await discoverDevices()
            }
            let updates = AsyncTimerSequence(interval: .seconds(2), clock: .continuous)
                .map { _ in
                    try await discoverDevices().groupedDevices().tableData
                }

            await Noora().table(collection.groupedDevices().tableData, updates: updates)
        }
    }
}

extension [DevicesCollection.GroupedDevice] {
    fileprivate var tableData: TableData {
        return TableData(
            columns: [
                TableColumn(title: "Name"),
                TableColumn(title: "Hostname"),
                TableColumn(title: "Interfaces"),
                TableColumn(title: "Version"),
            ],
            rows: self.map { device in
                var hostname = ""
                for case .lan(let lanDevice) in device.interfaces {
                    hostname = lanDevice.hostname
                }

                return [
                    "\(device.name)",
                    "\(hostname)",
                    "\(device.interfaces.map { $0.shortDescription }.joined(separator: ", "))",
                    "\(device.interfaces.compactMap(\.agentVersion).first ?? "Unknown")",
                ]
            }
        )
    }
}

extension DevicesCollection {
    private func resolveUSBDeviceAgentVersions() async -> [USBDevice] {
        // TODO: Agent version resolution unsupported
        return usbDevices
    }

    private func resolveEthernetDeviceAgentVersions() async -> [EthernetInterface] {
        // TODO: Agent version resolution unsupported
        return ethernetDevices
    }

    private func resolveLANDeviceAgentVersions() async -> [LANDevice] {
        await withTaskGroup(of: LANDevice?.self) { group in
            for device in lanDevices {
                group.addTask {
                    do {
                        return try await withGRPCClient(
                            AgentConnectionOptions.Endpoint(host: device.hostname, port: 50051),
                            security: .plaintext
                        ) { client in
                            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(
                                wrapping: client
                            )
                            let version = try await agent.getAgentVersion(
                                request: .init(message: .init())
                            )
                            var device = device
                            device.agentVersion = version.version
                            return device
                        }
                    } catch {
                        return device
                    }
                }
            }

            return await group.reduce(into: [LANDevice]()) { devices, device in
                if let device {
                    devices.append(device)
                }
            }
        }
    }

    func resolveAgentVersions() async throws -> DevicesCollection {
        return await withTaskGroup(of: DevicesCollection.self) { group in
            group.addTask {
                let devices = await resolveUSBDeviceAgentVersions()
                return DevicesCollection(usb: devices)
            }

            group.addTask {
                let devices = await resolveEthernetDeviceAgentVersions()
                return DevicesCollection(ethernet: devices)
            }

            group.addTask {
                let devices = await resolveLANDeviceAgentVersions()
                return DevicesCollection(lan: devices)
            }

            var collection = DevicesCollection()

            for await devices in group {
                collection.usbDevices.append(contentsOf: devices.usbDevices)
                collection.ethernetDevices.append(contentsOf: devices.ethernetDevices)
                collection.lanDevices.append(contentsOf: devices.lanDevices)
            }

            return collection
        }
    }
}
