import Foundation
import Logging
import Yams

/// Actor responsible for managing CDI specifications and containerd configuration
public actor CDIManager {
    private let specGenerator: CDISpecGenerator
    private let fileSystemProvider: any FileSystemProvider
    private let cdiSpecPath = "/etc/cdi"
    private let logger = Logger(label: "CDIManager")

    public init(
        specGenerator: CDISpecGenerator,
        fileSystemProvider: any FileSystemProvider = DefaultFileSystemProvider()
    ) {
        self.specGenerator = specGenerator
        self.fileSystemProvider = fileSystemProvider
    }

    /// Update CDI specifications based on current hardware discovery
    public func updateCDISpecs(for categories: [String]? = nil) async throws {
        logger.info(
            "Updating CDI specifications",
            metadata: [
                "categories": .string(categories?.joined(separator: ", ") ?? "all")
            ]
        )

        let specs = try await specGenerator.generateCDISpecs(for: categories)
        try writeCDISpecs(specs)

        logger.info(
            "Successfully updated CDI specifications",
            metadata: [
                "specs_written": .stringConvertible(specs.count)
            ]
        )
    }

    /// Get list of available CDI device identifiers
    public func getAvailableCDIDevices() throws -> [CDIDeviceInfo] {
        logger.debug("Scanning for available CDI devices")

        var devices: [CDIDeviceInfo] = []

        guard directoryExists(cdiSpecPath) else {
            logger.info(
                "CDI spec directory does not exist",
                metadata: [
                    "path": .string(cdiSpecPath)
                ]
            )
            return devices
        }

        let specFiles = try fileSystemProvider.contentsOfDirectory(atPath: cdiSpecPath)

        for fileName in specFiles where fileName.hasSuffix(".json") {
            let filePath = "\(cdiSpecPath)/\(fileName)"

            do {
                guard let jsonString = try fileSystemProvider.readFile(atPath: filePath),
                    let data = jsonString.data(using: .utf8)
                else {
                    continue
                }

                let spec = try JSONDecoder().decode(CDISpecification.self, from: data)

                for device in spec.devices {
                    let identifier = "\(spec.kind)=\(device.name)"
                    let deviceInfo = CDIDeviceInfo(
                        identifier: identifier,
                        category: extractCategoryFromDeviceName(device.name),
                        description: device.name,
                        devicePaths: device.containerEdits.deviceNodes?.map(\.path) ?? []
                    )
                    devices.append(deviceInfo)
                }
            } catch {
                logger.warning(
                    "Failed to parse CDI spec file",
                    metadata: [
                        "file": .string(fileName),
                        "error": .string(error.localizedDescription),
                    ]
                )
            }
        }

        logger.debug(
            "Found CDI devices",
            metadata: [
                "count": .stringConvertible(devices.count)
            ]
        )

        return devices
    }

    /// Get CDI devices filtered by category
    public func getCDIDevices(for categories: [String]) throws -> [CDIDeviceInfo] {
        let allDevices = try getAvailableCDIDevices()
        let filteredDevices = allDevices.filter { device in
            categories.contains { category in
                device.category.lowercased() == category.lowercased()
            }
        }

        logger.debug(
            "Filtered CDI devices by category",
            metadata: [
                "requested_categories": .string(categories.joined(separator: ", ")),
                "found_devices": .stringConvertible(filteredDevices.count),
            ]
        )

        return filteredDevices
    }

    /// Ensure CDI directory exists and has proper permissions
    public func ensureCDIDirectoryExists() throws {
        guard !directoryExists(cdiSpecPath) else {
            return
        }

        logger.info(
            "Creating CDI spec directory",
            metadata: [
                "path": .string(cdiSpecPath)
            ]
        )

        try FileManager.default.createDirectory(
            atPath: cdiSpecPath,
            withIntermediateDirectories: true,
            attributes: nil
        )
    }

    /// Remove all CDI specifications
    public func clearCDISpecs() throws {
        logger.info("Clearing all CDI specifications")

        guard directoryExists(cdiSpecPath) else {
            return
        }

        let specFiles = try fileSystemProvider.contentsOfDirectory(atPath: cdiSpecPath)

        for fileName in specFiles where fileName.hasSuffix(".json") {
            let filePath = "\(cdiSpecPath)/\(fileName)"
            try FileManager.default.removeItem(atPath: filePath)
            logger.debug(
                "Removed CDI spec file",
                metadata: [
                    "file": .string(fileName)
                ]
            )
        }
    }

    // MARK: - Private Methods

    private func directoryExists(_ path: String) -> Bool {
        var isDirectory: ObjCBool = false
        let exists = FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory)
        return exists && isDirectory.boolValue
    }

    private func writeCDISpecs(_ specs: [CDISpecification]) throws {
        try ensureCDIDirectoryExists()

        // Clear existing specs first
        try clearCDISpecs()

        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]

        for (index, spec) in specs.enumerated() {
            let fileName = "wendy-hardware-\(index).json"
            let filePath = "\(cdiSpecPath)/\(fileName)"

            let data = try encoder.encode(spec)
            try data.write(to: URL(fileURLWithPath: filePath))

            logger.debug(
                "Wrote CDI spec file",
                metadata: [
                    "file": .string(fileName),
                    "devices": .stringConvertible(spec.devices.count),
                ]
            )
        }
    }

    private func extractCategoryFromDeviceName(_ deviceName: String) -> String {
        // Extract category from device name like "gpio-gpiochip0"
        if let dashIndex = deviceName.firstIndex(of: "-") {
            return String(deviceName[..<dashIndex]).uppercased()
        }
        return "UNKNOWN"
    }

    /// Load NVIDIA CDI specs from /etc/cdi or /var/run/cdi (YAML format)
    public func loadNVIDIACDISpec(deviceName: String = "all") throws -> CDISpecification {
        logger.debug("Loading NVIDIA CDI spec", metadata: ["device": .string(deviceName)])

        // Try multiple common locations for NVIDIA CDI specs
        let possiblePaths = [
            "\(cdiSpecPath)/nvidia.yaml",  // /etc/cdi/nvidia.yaml (Jetson)
            "/var/run/cdi/nvidia.yaml",  // /var/run/cdi/nvidia.yaml (datacenter)
        ]

        var nvidiaSpecPath: String?
        for path in possiblePaths {
            if fileSystemProvider.fileExists(atPath: path) {
                nvidiaSpecPath = path
                break
            }
        }

        guard let specPath = nvidiaSpecPath else {
            throw CDIError.specNotFound(possiblePaths.joined(separator: ", "))
        }

        guard let yamlString = try fileSystemProvider.readFile(atPath: specPath) else {
            throw CDIError.cannotReadSpec(specPath)
        }

        // Parse YAML
        let decoder = YAMLDecoder()
        let spec = try decoder.decode(CDISpecification.self, from: yamlString)

        logger.debug(
            "Loaded NVIDIA CDI spec",
            metadata: [
                "path": .string(specPath),
                "kind": .string(spec.kind),
                "devices": .stringConvertible(spec.devices.count),
            ]
        )

        return spec
    }
}

enum CDIError: Error, CustomStringConvertible {
    case specNotFound(String)
    case cannotReadSpec(String)
    case deviceNotFound(String)
    case failedToStatDevice(String)

    var description: String {
        switch self {
        case .specNotFound(let path):
            return "CDI spec not found at \(path)"
        case .cannotReadSpec(let path):
            return "Cannot read CDI spec at \(path)"
        case .deviceNotFound(let name):
            return "Device '\(name)' not found in CDI spec"
        case .failedToStatDevice(let path):
            return "Failed to stat device at \(path)"
        }
    }
}

// MARK: - CDI Device Info

/// Information about a CDI device
public struct CDIDeviceInfo: Sendable {
    let identifier: String
    let category: String
    let description: String
    let devicePaths: [String]

    public init(identifier: String, category: String, description: String, devicePaths: [String]) {
        self.identifier = identifier
        self.category = category
        self.description = description
        self.devicePaths = devicePaths
    }
}

// MARK: - Containerd Configuration Manager

/// Manages containerd configuration for CDI support
public struct ContainerdConfigManager: Sendable {
    private let configPath = "/etc/containerd/config.toml"
    private let fileSystemProvider: any FileSystemProvider
    private let logger = Logger(label: "ContainerdConfigManager")

    public init(fileSystemProvider: any FileSystemProvider = DefaultFileSystemProvider()) {
        self.fileSystemProvider = fileSystemProvider
    }

    /// Ensure CDI is enabled in containerd configuration
    public func ensureCDIEnabled() throws {
        logger.info("Checking containerd CDI configuration")

        let config = try readContainerdConfig()
        let updatedConfig = enableCDIInConfig(config)

        if config != updatedConfig {
            try writeContainerdConfig(updatedConfig)
            logger.info("Updated containerd configuration to enable CDI")
        } else {
            logger.debug("CDI already enabled in containerd configuration")
        }
    }

    private func readContainerdConfig() throws -> String {
        guard fileSystemProvider.fileExists(atPath: configPath) else {
            logger.info(
                "Containerd config file does not exist, creating default",
                metadata: [
                    "path": .string(configPath)
                ]
            )
            return createDefaultContainerdConfig()
        }

        return try fileSystemProvider.readFile(atPath: configPath) ?? ""
    }

    private func writeContainerdConfig(_ config: String) throws {
        let data = config.data(using: .utf8) ?? Data()
        try data.write(to: URL(fileURLWithPath: configPath))
    }

    private func enableCDIInConfig(_ config: String) -> String {
        // Check if CDI is already configured
        if config.contains("[plugins.\"io.containerd.grpc.v1.cri\".cdi]") {
            return config
        }

        let cdiConfig = """

            [plugins."io.containerd.grpc.v1.cri".cdi]
              enable_cdi = true
              cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]
            """

        // Add CDI configuration to the end of the file
        return config + cdiConfig
    }

    private func createDefaultContainerdConfig() -> String {
        return """
            version = 2

            [plugins."io.containerd.grpc.v1.cri"]
              sandbox_image = "k8s.gcr.io/pause:3.6"

            [plugins."io.containerd.grpc.v1.cri".cdi]
              enable_cdi = true
              cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]
            """
    }
}
