import ArgumentParser
import Foundation
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

struct DiscoverCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "discover",
        abstract: "List USB and Ethernet devices connected to the system"
    )

    enum DeviceType: String, ExpressibleByArgument {
        case usb, ethernet, lan, all
    }

    @Option(help: "Device types to list (usb, ethernet, lan, or all)")
    var type: DeviceType = .all

    @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
    var json: Bool = false

    @Flag(help: "Skip resolving the agent's version")
    var skipResolveAgentVersion: Bool = false

    // Helper method for logging device counts
    private func logDevicesFound<T: Device>(_ devices: [T], deviceType: String, logger: Logger) {
        if json {
            return
        }

        if devices.isEmpty {
            Noora().info("No Wendy \(deviceType) found.")
        } else {
            Noora().success("Found \(devices.count) Wendy \(deviceType)")
        }
    }

    func run() async throws {
        let logger = Logger(label: "sh.wendy.cli.devices")
        let discovery = PlatformDeviceDiscovery(logger: logger)
        let format = json ? OutputFormat.json : OutputFormat.text

        // Collect devices based on the requested type
        var usbDevices: [USBDevice] = []
        var ethernetDevices: [EthernetInterface] = []
        var lanDevices: [LANDevice] = []

        switch type {
        case .usb:
            usbDevices = try await Noora().progressStep(
                message: "Discovering Wendy USB devices",
                successMessage: nil,
                errorMessage: nil,
                showSpinner: !json
            ) { progress in
                async let _usbDevices = await discovery.findUSBDevices()
                let usb = await _usbDevices
                return usb
            }
            logDevicesFound(usbDevices, deviceType: "USB device(s)", logger: logger)

        case .ethernet:
            ethernetDevices = try await Noora().progressStep(
                message: "Discovering Wendy Ethernet interfaces",
                successMessage: nil,
                errorMessage: nil,
                showSpinner: !json
            ) { progress in
                async let _ethernetDevices = await discovery.findEthernetInterfaces()
                let ethernet = await _ethernetDevices
                return ethernet
            }
            logDevicesFound(ethernetDevices, deviceType: "Ethernet interface(s)", logger: logger)

        case .lan:
            lanDevices = try await Noora().progressStep(
                message: "Discovering Wendy LAN devices",
                successMessage: nil,
                errorMessage: nil,
                showSpinner: !json
            ) { progress in
                async let _lanDevices = try await discovery.findLANDevices()
                let lan = try await _lanDevices
                return lan
            }
            logDevicesFound(lanDevices, deviceType: "LAN device(s)", logger: logger)

        case .all:
            // Fetch all types of devices
            let devices = try await Noora().progressStep(
                message: "Discovering all Wendy devices",
                successMessage: nil,
                errorMessage: nil,
                showSpinner: !json
            ) { progress in
                async let _usbDevices = await discovery.findUSBDevices()
                async let _ethernetDevices = await discovery.findEthernetInterfaces()
                async let _lanDevices = try await discovery.findLANDevices()

                let usb = await _usbDevices
                let ethernet = await _ethernetDevices
                let lan = try await _lanDevices

                return (usb, ethernet, lan)
            }

            usbDevices = devices.0
            ethernetDevices = devices.1
            lanDevices = devices.2

            logDevicesFound(usbDevices, deviceType: "USB device(s)", logger: logger)
            logDevicesFound(ethernetDevices, deviceType: "Ethernet interface(s)", logger: logger)
            logDevicesFound(lanDevices, deviceType: "LAN device(s)", logger: logger)
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

        if format == .json {
            do {
                let jsonOutput = try collection.toJSON()
                print(jsonOutput)
            } catch {
                logger.error("Error serializing to JSON: \(error)")
            }
        } else {
            print(collection.toHumanReadableString())
        }
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
