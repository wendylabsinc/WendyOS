import ArgumentParser
import Foundation
import Logging
import WendyShared

@available(macOS 14.0, *)
struct WendyHelper: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "wendy-helper",
        abstract: "Wendy USB device monitoring daemon",
        discussion: """
            A background daemon that monitors for Wendy USB devices and automatically
            configures network interfaces with unique IP addresses.
            """,
        version: "1.0.0"
    )

    @Flag(name: .shortAndLong, help: "Run in foreground (don't daemonize)")
    var foreground = false

    @Flag(name: .shortAndLong, help: "Enable debug logging")
    var debug = false

    @Option(name: .shortAndLong, help: "Log file path")
    var logFile: String?

    func run() async throws {
        // Set up logging
        let logLevel: Logger.Level = debug ? .debug : .info
        LoggingSystem.bootstrap { label in
            var handler = StreamLogHandler.standardOutput(label: label)
            handler.logLevel = logLevel
            return handler
        }

        let logger = Logger(label: "sh.wendy-helper")

        logger.info("Starting Wendy Helper Daemon")
        logger.info("Version: \(Version.current)")
        logger.info("Foreground mode: \(foreground)")
        logger.info("Debug logging: \(debug)")

        let deviceDiscovery = PlatformDeviceDiscovery(logger: logger)

        // Use event-driven monitoring on macOS, polling on other platforms
        let usbMonitor: USBMonitorService
        let networkDaemonClient = NetworkDaemonClient(logger: logger)
        let ipManager = PlatformIPAddressManager(logger: logger)

        // Create the main daemon service
        let daemon: WendyHelperDaemon
        #if os(macOS)
            usbMonitor = IOKitUSBMonitor(logger: logger)
            logger.info("Using IOKit event-driven USB monitoring")
            let networkInterfaceMonitor = NetworkInterfaceMonitor(logger: logger)
            logger.info("Using SystemConfiguration event-driven network interface monitoring")

            daemon = WendyHelperDaemon(
                usbMonitor: usbMonitor,
                deviceDiscovery: deviceDiscovery,
                networkDaemonClient: networkDaemonClient,
                ipManager: ipManager,
                logger: logger,
                networkInterfaceMonitor: networkInterfaceMonitor
            )
        #else
            usbMonitor = PlatformUSBMonitor(
                deviceDiscovery: deviceDiscovery,
                logger: logger,
                pollingInterval: .seconds(30)
            )
            logger.info("Using polling-based USB monitoring")

            daemon = WendyHelperDaemon(
                usbMonitor: usbMonitor,
                deviceDiscovery: deviceDiscovery,
                networkDaemonClient: networkDaemonClient,
                ipManager: ipManager,
                logger: logger
            )
        #endif

        logger.info("Initializing daemon services...")
        try await daemon.start()

        // Keep the daemon running
        logger.info("Wendy Helper Daemon is running. Press Ctrl+C to stop.")

        // Run indefinitely until cancelled
        do {
            while true {
                try await Task.sleep(for: .seconds(60))
            }
        } catch {
            logger.info("Daemon stopped")
            await daemon.stop()
        }
    }
}

/// Main daemon service that coordinates USB monitoring and network configuration via XPC
/// Protocol for network daemon client functionality
protocol NetworkDaemonClientProtocol: Actor {
    func configureInterface(
        name: String,
        bsdName: String,
        deviceId: String,
        ipAddress: String,
        subnetMask: String,
        gateway: String?
    ) async throws

    func isInterfaceConfigured(
        name: String,
        bsdName: String,
        deviceId: String
    ) async throws -> Bool

    func cleanupInterface(
        name: String,
        bsdName: String,
        deviceId: String
    ) async throws
}

@available(macOS 14.0, *)
actor WendyHelperDaemon {
    private let logger: Logger
    private let usbMonitor: USBMonitorService
    private let deviceDiscovery: DeviceDiscovery
    private let networkDaemonClient: any NetworkDaemonClientProtocol
    private let ipManager: IPAddressManager
    #if os(macOS)
        private let networkInterfaceMonitor: NetworkInterfaceMonitorProtocol?
    #endif
    private var isRunning = false
    private var pendingUSBDevices: [String: USBDeviceInfo] = [:]

    #if os(macOS)
        init(
            usbMonitor: USBMonitorService,
            deviceDiscovery: DeviceDiscovery,
            networkDaemonClient: any NetworkDaemonClientProtocol,
            ipManager: IPAddressManager,
            logger: Logger,
            networkInterfaceMonitor: NetworkInterfaceMonitorProtocol? = nil
        ) {
            self.logger = logger
            self.usbMonitor = usbMonitor
            self.deviceDiscovery = deviceDiscovery
            self.networkDaemonClient = networkDaemonClient
            self.ipManager = ipManager
            self.networkInterfaceMonitor = networkInterfaceMonitor
        }
    #else
        init(
            usbMonitor: USBMonitorService,
            deviceDiscovery: DeviceDiscovery,
            networkDaemonClient: any NetworkDaemonClientProtocol,
            ipManager: IPAddressManager,
            logger: Logger
        ) {
            self.logger = logger
            self.usbMonitor = usbMonitor
            self.deviceDiscovery = deviceDiscovery
            self.networkDaemonClient = networkDaemonClient
            self.ipManager = ipManager
        }
    #endif

    func start() async throws {
        guard !isRunning else { return }

        logger.info("Starting Wendy Helper Daemon services...")

        // Initialize IP manager
        try await ipManager.initialize()

        // Set up device event handling
        await usbMonitor.setDeviceHandler { [weak self] event in
            Task { await self?.handleDeviceEvent(event) }
        }

        #if os(macOS)
            // Set up network interface event handling
            if let networkInterfaceMonitor = networkInterfaceMonitor {
                await networkInterfaceMonitor.setInterfaceHandler { [weak self] event in
                    Task { await self?.handleNetworkInterfaceEvent(event) }
                }

                // Start network interface monitoring
                try await networkInterfaceMonitor.start()
            }
        #endif

        // Start USB monitoring
        do {
            try await usbMonitor.start()
        } catch {
            logger.error("Failed to start USB monitoring: \(error)")
            isRunning = false
            return
        }

        isRunning = true
        logger.info("Wendy Helper Daemon started")
    }

    func stop() async {
        guard isRunning else { return }

        await usbMonitor.stop()

        #if os(macOS)
            if let networkInterfaceMonitor = networkInterfaceMonitor {
                await networkInterfaceMonitor.stop()
            }
        #endif

        pendingUSBDevices.removeAll()
        isRunning = false
        logger.info("Wendy Helper Daemon stopped")
    }

    private func handleDeviceEvent(_ event: USBDeviceEvent) async {
        logger.info("Handling device event: \(event)")
        switch event {
        case .connected(let device) where device.isWendyDevice:
            logger.info("Connected Wendy device: \(device.name)")

            #if os(macOS)
                // On macOS, wait for network interface to appear via SystemConfiguration events
                pendingUSBDevices[device.id] = device
                logger.debug("Stored pending Wendy device: \(device.name)")
            #else
                // On other platforms, use the retry mechanism
                await configureWendyDevice(device)
            #endif

        case .disconnected(let device) where device.isWendyDevice:
            logger.info("Disconnected Wendy device: \(device.name)")

            #if os(macOS)
                // Remove from pending if it was there
                pendingUSBDevices.removeValue(forKey: device.id)
            #endif

            await cleanupWendyDevice(device)
        default:
            break
        }
    }

    #if os(macOS)
        private func handleNetworkInterfaceEvent(_ event: NetworkInterfaceEvent) async {
            logger.info("Handling network interface event: \(event)")

            switch event {
            case .interfaceAppeared(let interfaceName):
                logger.info("Wendy network interface appeared: \(interfaceName)")

                // Check if we have a pending USB device that matches this interface
                for (deviceId, device) in pendingUSBDevices {
                    logger.debug(
                        "Attempting to configure pending device: \(device.name) for interface: \(interfaceName)"
                    )
                    await configureWendyDevice(device)

                    // Remove from pending after attempting configuration
                    pendingUSBDevices.removeValue(forKey: deviceId)
                    break  // Configure one device at a time
                }

            case .interfaceDisappeared(let interfaceName):
                logger.info("Wendy network interface disappeared: \(interfaceName)")
            // Interface cleanup is handled by USB disconnect events
            }
        }
    #endif

    private func configureWendyDevice(_ device: USBDeviceInfo) async {
        logger.debug("Configuring Wendy device", metadata: ["device": .string(device.name)])

        do {
            // 1. Find network interfaces for this device
            let ethernetInterfaces = await deviceDiscovery.findEthernetInterfaces()
            let wendyOSInterfaces = ethernetInterfaces.filter { $0.isWendyDevice }

            let interfaces = wendyOSInterfaces.map { interface in
                (name: interface.displayName, bsdName: interface.name, deviceId: device.id)
            }

            logger.debug(
                "Found \(interfaces.count) interfaces for device",
                metadata: [
                    "device": .string(device.name),
                    "interfaces": .array(interfaces.map { .string($0.bsdName) }),
                ]
            )

            // 2. Configure each interface via XPC
            for interface in interfaces {
                // Check if already configured via network daemon
                let isConfigured = try await networkDaemonClient.isInterfaceConfigured(
                    name: interface.name,
                    bsdName: interface.bsdName,
                    deviceId: interface.deviceId
                )

                if isConfigured {
                    logger.debug(
                        "Interface \(interface.bsdName) already configured",
                        metadata: [
                            "device": .string(device.name), "interface": .string(interface.bsdName),
                        ]
                    )
                    continue
                }

                // 3. Assign IP address
                let networkInterface = NetworkInterface(
                    name: interface.name,
                    bsdName: interface.bsdName,
                    deviceId: interface.deviceId
                )
                let ipConfig = try await ipManager.assignIPAddress(for: networkInterface)

                // 4. Apply network configuration via XPC to privileged daemon
                try await networkDaemonClient.configureInterface(
                    name: interface.name,
                    bsdName: interface.bsdName,
                    deviceId: interface.deviceId,
                    ipAddress: ipConfig.ipAddress,
                    subnetMask: ipConfig.subnetMask,
                    gateway: ipConfig.gateway
                )

                logger.debug(
                    "Configured interface \(interface.bsdName) with IP \(ipConfig.ipAddress)",
                    metadata: [
                        "device": .string(device.name), "interface": .string(interface.bsdName),
                        "ip": .string(ipConfig.ipAddress),
                    ]
                )
            }
        } catch {
            logger.error("Failed to configure Wendy device: \(error)")
        }
    }

    private func cleanupWendyDevice(_ device: USBDeviceInfo) async {
        logger.info("Cleaning up Wendy device: \(device.name)")

        // Find and cleanup interfaces via XPC
        let ethernetInterfaces = await deviceDiscovery.findEthernetInterfaces()
        let wendyOSInterfaces = ethernetInterfaces.filter { $0.isWendyDevice }

        for interface in wendyOSInterfaces {
            do {
                // Cleanup via network daemon
                try await networkDaemonClient.cleanupInterface(
                    name: interface.displayName,
                    bsdName: interface.name,
                    deviceId: device.id
                )

                // Release IP from local manager
                let networkInterface = NetworkInterface(
                    name: interface.displayName,
                    bsdName: interface.name,
                    deviceId: device.id
                )
                await ipManager.releaseIPAddress(for: networkInterface)

                logger.info("Cleaned up interface \(interface.name)")
            } catch {
                logger.error("Failed to cleanup interface \(interface.name): \(error)")
            }
        }
    }
}

@available(macOS 14.0, *)
@main
enum Main {
    static func main() async {
        await WendyHelper.main()
    }
}
