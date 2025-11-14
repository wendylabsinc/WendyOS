import Foundation
import Logging

// MARK: - CDI Data Structures

public struct CDISpecification: Codable, Sendable {
    let cdiVersion: String
    let kind: String
    let containerEdits: CDIContainerEdits?
    let devices: [CDIDevice]

    public init(
        devices: [CDIDevice],
        cdiVersion: String = "0.6.0",
        kind: String = "edge.com/hardware",  // TODO: What's this?
        containerEdits: CDIContainerEdits? = nil
    ) {
        self.cdiVersion = cdiVersion
        self.kind = kind
        self.containerEdits = containerEdits
        self.devices = devices
    }
}

public struct CDIDevice: Codable, Sendable {
    let name: String
    let containerEdits: CDIContainerEdits

    public init(name: String, containerEdits: CDIContainerEdits) {
        self.name = name
        self.containerEdits = containerEdits
    }
}

public struct CDIContainerEdits: Codable, Sendable {
    let deviceNodes: [CDIDeviceNode]?
    let mounts: [CDIMount]?
    let env: [String]?
    let hooks: [CDIHook]?

    public init(
        deviceNodes: [CDIDeviceNode]? = nil,
        mounts: [CDIMount]? = nil,
        env: [String]? = nil,
        hooks: [CDIHook]? = nil
    ) {
        self.deviceNodes = deviceNodes
        self.mounts = mounts
        self.env = env
        self.hooks = hooks
    }
}

public struct CDIDeviceNode: Codable, Sendable {
    let path: String
    let hostPath: String?
    let type: String?
    let major: Int?
    let minor: Int?
    let fileMode: Int?
    let permissions: String?

    public init(
        path: String,
        hostPath: String? = nil,
        type: String? = nil,
        major: Int? = nil,
        minor: Int? = nil,
        fileMode: Int? = nil,
        permissions: String? = nil
    ) {
        self.path = path
        self.hostPath = hostPath
        self.type = type
        self.major = major
        self.minor = minor
        self.fileMode = fileMode
        self.permissions = permissions
    }

    /// The actual host path (defaults to container path if not specified)
    var effectiveHostPath: String {
        return hostPath ?? path
    }
}

public struct CDIMount: Codable, Sendable {
    let hostPath: String
    let containerPath: String
    let type: String?
    let options: [String]?

    public init(
        hostPath: String,
        containerPath: String,
        type: String? = nil,
        options: [String]? = nil
    ) {
        self.hostPath = hostPath
        self.containerPath = containerPath
        self.type = type
        self.options = options
    }
}

public struct CDIHook: Codable, Sendable {
    let hookName: String
    let path: String
    let args: [String]?
    let env: [String]?
    let timeout: Int?

    public init(
        hookName: String,
        path: String,
        args: [String]? = nil,
        env: [String]? = nil,
        timeout: Int? = nil
    ) {
        self.hookName = hookName
        self.path = path
        self.args = args
        self.env = env
        self.timeout = timeout
    }
}

// MARK: - CDI Specification Generator

public struct CDISpecGenerator: Sendable {
    private let hardwareDiscoverer: SystemHardwareDiscoverer
    private let logger = Logger(label: "CDISpecGenerator")

    public init(hardwareDiscoverer: SystemHardwareDiscoverer) {
        self.hardwareDiscoverer = hardwareDiscoverer
    }

    public func generateCDISpecs(for categories: [String]? = nil) async throws -> [CDISpecification]
    {
        logger.info(
            "Generating CDI specifications",
            metadata: [
                "categories": .string(categories?.joined(separator: ", ") ?? "all")
            ]
        )

        let capabilities = try await hardwareDiscoverer.discoverCapabilities(
            categoryFilter: categories?.first
        )
        logger.debug(
            "Discovered hardware capabilities",
            metadata: [
                "count": .stringConvertible(capabilities.count)
            ]
        )

        let specs = groupCapabilitiesIntoCDISpecs(capabilities)

        logger.info(
            "Generated CDI specifications",
            metadata: [
                "specs_count": .stringConvertible(specs.count),
                "total_devices": .stringConvertible(specs.reduce(0) { $0 + $1.devices.count }),
            ]
        )

        return specs
    }

    private func groupCapabilitiesIntoCDISpecs(
        _ capabilities: [HardwareCapability]
    ) -> [CDISpecification] {
        // Group capabilities by category
        let groupedCapabilities = Dictionary(grouping: capabilities) { $0.category }

        return groupedCapabilities.compactMap { (category, capabilities) in
            let devices = capabilities.compactMap { capability in
                createCDIDevice(from: capability)
            }

            guard !devices.isEmpty else { return nil }

            return CDISpecification(devices: devices)
        }
    }

    private func createCDIDevice(from capability: HardwareCapability) -> CDIDevice? {
        guard !capability.devicePath.isEmpty else {
            logger.debug(
                "Skipping capability without device path",
                metadata: [
                    "category": .string(capability.category),
                    "description": .string(capability.description),
                ]
            )
            return nil
        }

        let deviceName = generateDeviceName(from: capability)
        let containerEdits = createContainerEdits(from: capability)

        return CDIDevice(name: deviceName, containerEdits: containerEdits)
    }

    private func generateDeviceName(from capability: HardwareCapability) -> String {
        let category = capability.category.lowercased()

        // Extract device identifier from path
        let devicePath = capability.devicePath
        if let lastComponent = devicePath.split(separator: "/").last {
            return "\(category)-\(lastComponent)"
        }

        // Fallback to hash-based name
        let hash = abs(devicePath.hashValue) % 10000
        return "\(category)-\(hash)"
    }

    private func createContainerEdits(from capability: HardwareCapability) -> CDIContainerEdits {
        var deviceNodes: [CDIDeviceNode] = []
        var mounts: [CDIMount] = []
        var env: [String] = []

        // Create device node for the primary device path
        deviceNodes.append(
            CDIDeviceNode(
                path: capability.devicePath,
                hostPath: capability.devicePath,
                permissions: getPermissionsForCategory(capability.category)
            )
        )

        // Add category-specific configurations
        switch capability.category.uppercased() {
        case "GPIO":
            // GPIO often needs access to /sys/class/gpio
            mounts.append(
                CDIMount(
                    hostPath: "/sys/class/gpio",
                    containerPath: "/sys/class/gpio",
                    type: "bind",
                    options: ["bind", "rw"]
                )
            )

        case "I2C", "SPI":
            // I2C/SPI may need access to sysfs entries
            if extractBusNumber(from: capability.devicePath) != nil {
                let sysPath = "/sys/bus/\(capability.category.lowercased())/devices"
                mounts.append(
                    CDIMount(
                        hostPath: sysPath,
                        containerPath: sysPath,
                        type: "bind",
                        options: ["bind", "ro"]
                    )
                )
            }

        case "GPU":
            // GPU devices may need additional paths
            if capability.devicePath.contains("nvidia") {
                // NVIDIA GPU specific mounts
                let additionalPaths = ["/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-modeset"]
                for path in additionalPaths {
                    deviceNodes.append(
                        CDIDeviceNode(
                            path: path,
                            hostPath: path,
                            permissions: "rw"
                        )
                    )
                }

                // Add NVIDIA libraries mount
                mounts.append(
                    CDIMount(
                        hostPath: "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
                        containerPath: "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
                        type: "bind",
                        options: ["bind", "ro"]
                    )
                )

                env.append("NVIDIA_VISIBLE_DEVICES=all")
                env.append("NVIDIA_DRIVER_CAPABILITIES=all")
            }

        case "CAMERA":
            // Camera devices may need video group access
            env.append("VIDEO_GROUP_ACCESS=true")

        default:
            break
        }

        return CDIContainerEdits(
            deviceNodes: deviceNodes.isEmpty ? nil : deviceNodes,
            mounts: mounts.isEmpty ? nil : mounts,
            env: env.isEmpty ? nil : env,
            hooks: nil
        )
    }

    private func getPermissionsForCategory(_ category: String) -> String {
        switch category.uppercased() {
        case "GPIO", "I2C", "SPI", "SERIAL":
            return "rw"
        case "GPU", "CAMERA", "AUDIO":
            return "rw"
        case "USB":
            return "rw"
        default:
            return "r"
        }
    }

    private func extractBusNumber(from devicePath: String) -> String? {
        // Extract bus number from paths like /dev/i2c-1, /dev/spidev0.0 using Swift Regex
        if let m = devicePath.firstMatch(of: /i2c-(\d+)/) { return String(m.1) }
        if let m = devicePath.firstMatch(of: /spidev(\d+)/) { return String(m.1) }
        if let m = devicePath.firstMatch(of: /ttyUSB(\d+)/) { return String(m.1) }
        if let m = devicePath.firstMatch(of: /ttyACM(\d+)/) { return String(m.1) }
        return nil
    }
}
