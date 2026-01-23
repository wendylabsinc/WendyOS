import AppConfig
import ArgumentParser
import Foundation
import Logging
import Noora
import SystemPackage
import WendyAgentGRPC

struct ProjectCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "project",
        abstract: "Manage Wendy projects",
        subcommands: [
            InitCommand.self,
            EntitlementsCommand.self,
        ]
    )
}

struct EntitlementsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "entitlements",
        abstract: "Manage project entitlements",
        subcommands: [
            ListCommand.self,
            AddCommand.self,
            RemoveCommand.self,
        ]
    )
}

protocol ModifyProjectCommand: AsyncParsableCommand {
    var project: String { get }
}

extension ModifyProjectCommand {
    func getWendyJsonPath() -> String {
        if project.hasSuffix("/") {
            return "\(project)wendy.json"
        } else {
            return "\(project)/wendy.json"
        }
    }

    func loadConfig(from path: String) throws -> AppConfig {
        let data = try Data(contentsOf: URL(fileURLWithPath: path))
        return try JSONDecoder().decode(AppConfig.self, from: data)
    }

    func saveConfig(_ config: AppConfig, to path: String) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(config)
        try data.write(to: URL(fileURLWithPath: path))
    }
}

// MARK: - List Command

struct ListCommand: ModifyProjectCommand {
    static let configuration = CommandConfiguration(
        commandName: "list",
        abstract: "List project entitlements"
    )

    @Flag(name: [.customShort("a"), .long], help: "Show all entitlements (enabled and disabled)")
    var showAll: Bool = false

    @Option(
        help: "Path to the project directory (defaults to current directory)"
    )
    var project: String = "."

    private var logger: Logger {
        Logger(label: "sh.wendy.cli.project.entitlements.list")
    }

    func run() async throws {
        let wendyJsonPath = getWendyJsonPath()

        // Check if wendy.json exists
        guard FileManager.default.fileExists(atPath: wendyJsonPath) else {
            if JSONMode.isEnabled {
                JSONErrorResponse(
                    error: "config_not_found",
                    reason: "No wendy.json found in current directory",
                    suggestion: "Run 'wendy project init' to initialize a new project"
                ).print()
            } else {
                print("❌ No wendy.json found in current directory")
                print("Run 'wendy project init' to initialize a new project")
            }
            throw CLIError.configNotFound(path: wendyJsonPath)
        }

        // Load configuration
        let config = try loadConfig(from: wendyJsonPath)

        // Get all available entitlement types
        let allEntitlementTypes: [EntitlementType] = [.network, .bluetooth, .video]

        if showAll {
            // Show all entitlements with status
            print("Project: \(config.appId)")
            print("Version: \(config.version)")
            print("")
            print("📋 Project Entitlements (all):")

            for entitlementType in allEntitlementTypes {
                let isEnabled = config.entitlements.contains { entitlement in
                    entitlementType == entitlement.type
                }

                let status = isEnabled ? "✅" : "❌"
                let statusText = isEnabled ? "enabled" : "disabled"
                print("\(status) \(entitlementType.rawValue.capitalized) (\(statusText))")

                // Show details for enabled entitlements
                if isEnabled {
                    if let entitlement = config.entitlements.first(where: {
                        $0.type == entitlementType
                    }) {
                        printEntitlementDetails(entitlement)
                    }
                }
                print("")
            }
        } else {
            // Show only enabled entitlements
            print("Project: \(config.appId)")
            print("Version: \(config.version)")
            print("")

            if config.entitlements.isEmpty {
                print("No entitlements configured")
                print("Use 'wendy project entitlements add <type>' to add entitlements")
            } else {
                print("📋 Project Entitlements:")
                for entitlement in config.entitlements {
                    print("✅ \(entitlement.type.rawValue.capitalized)")
                    printEntitlementDetails(entitlement)
                    print("")
                }
            }
        }
    }

    private func printEntitlementDetails(_ entitlement: Entitlement) {
        switch entitlement {
        case .network(let networkEntitlement):
            print("   Mode: \(networkEntitlement.mode.rawValue)")
        case .bluetooth(let bluetoothEntitlement):
            print("   Mode: \(bluetoothEntitlement.mode.rawValue)")
        case .video(let videoEntitlement):
            print("   Mode: \(videoEntitlement.mode.rawValue)")
            switch videoEntitlement.mode {
            case .all:
                print("   All detected video devices")
            case .allowlist:
                print("   Selected Video Devices:")
                for allowlist in videoEntitlement.allowlist {
                    print("      - \(allowlist)")
                }
            }
        case .audio:
            print("   No additional configuration")
        case .gpu:
            print("   No additional configuration")
        case .persist(let persistenceEntitlement):
            print("   Name: \(persistenceEntitlement.name)")
            print("   Path: \(persistenceEntitlement.path)")
        }
    }
}

// MARK: - Add Command

struct AddCommand: ModifyProjectCommand {
    static let configuration = CommandConfiguration(
        commandName: "add",
        abstract: "Add an entitlement to the project"
    )

    @Option(help: "Type of entitlement to add (network, bluetooth, video)")
    var entitlementType: EntitlementType?

    @Option(name: [.customShort("m"), .long], help: "Mode for the entitlement")
    var mode: String?

    @Option(help: "Name of the volume to persist")
    var name: String?

    @Option(help: "Path of the directory to persist")
    var path: String?

    @Option(
        help: "Path to the project directory (defaults to current directory)"
    )
    var project: String = "."

    private var logger: Logger {
        Logger(label: "sh.wendy.cli.project.entitlements.add")
    }

    func run() async throws {
        let wendyJsonPath = getWendyJsonPath()

        // Check if wendy.json exists
        guard FileManager.default.fileExists(atPath: wendyJsonPath) else {
            Noora().warning(
                """
                No wendy.json found in current directory
                Run 'wendy project init' to initialize a new project
                """
            )
            throw CLIError.configNotFound(path: wendyJsonPath)
        }

        // Load current configuration
        var config = try loadConfig(from: wendyJsonPath)
        let newEntitlement: Entitlement

        if let entitlementType {
            // Check if entitlement already exists
            if config.entitlements.contains(where: { $0.type == entitlementType }) {
                Noora().warning(
                    "\(entitlementType.rawValue.capitalized) entitlement already exists"
                )
                return
            }

            // Create new entitlement based on type and mode
            newEntitlement = try createEntitlement(type: entitlementType, mode: mode)
        } else {
            var availableEntitlementTypes = EntitlementType.allCases.filter { entitlement in
                !config.entitlements.contains { $0.type == entitlement }
            }

            if !availableEntitlementTypes.contains(.persist) {
                // Persist entitlement is always available, add it to the list regardless
                availableEntitlementTypes.append(.persist)
            }

            if availableEntitlementTypes.isEmpty {
                Noora().info("All entitlements are already enabled")
                return
            }

            Noora().info("Select an entitlement to enable")

            let index = try await Noora().selectableTable(
                headers: [
                    .primary("Entitlement")
                ],
                rows: availableEntitlementTypes.map { entitlement in
                    return [
                        .plain(entitlement.rawValue.capitalized)
                    ]
                },
                pageSize: EntitlementType.allCases.count
            )

            switch availableEntitlementTypes[index] {
            case .network:
                let host = Noora().yesOrNoChoicePrompt(
                    question: TerminalText("Do you want to allow host network access?")
                )

                if host {
                    newEntitlement = .network(NetworkEntitlements(mode: .host))
                } else {
                    newEntitlement = .network(NetworkEntitlements(mode: .none))
                }
            case .bluetooth:
                let bluez = Noora().yesOrNoChoicePrompt(
                    question: TerminalText("Do you want to use bluez?")
                )
                newEntitlement = .bluetooth(
                    BluetoothEntitlements(
                        mode: bluez ? .bluez : .kernel
                    )
                )
            case .video:
                let mode = Noora().singleChoicePrompt(
                    question: "Which devices do you want to allow?",
                    options: VideoEntitlements.VideoMode.allCases
                )

                switch mode {
                case .all:
                    newEntitlement = .video(VideoEntitlements(mode: .all))
                case .allowlist:
                    let devices = try await withAgentGRPCClient(
                        AgentConnectionOptions(endpoint: nil),
                        title: TerminalText("Select a WendyOS device to discover video inputs")
                    ) { client in
                        let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(
                            wrapping: client
                        )

                        var request = Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest()
                        request.categoryFilter = "camera"

                        return try await agent.listHardwareCapabilities(request).capabilities
                    }

                    if devices.isEmpty {
                        Noora().warning("No camera devices found")
                        return
                    } else {
                        let allowlist = Noora().multipleChoicePrompt(
                            question: "Which device(s) do you want to allow?",
                            options: devices.map { $0.devicePath }
                        )

                        newEntitlement = .video(
                            VideoEntitlements(mode: .allowlist, allowlist: allowlist)
                        )
                    }
                }
            case .audio:
                newEntitlement = .audio
            case .gpu:
                newEntitlement = .gpu(GPUEntitlements())
            case .persist:
                let name = Noora().textPrompt(
                    prompt: "Enter the name of the volume to persist"
                )
                let path = Noora().textPrompt(
                    prompt: "Enter the path of the directory to persist"
                )

                // TODO: Validate `path` is a valid UNIX path?
                newEntitlement = .persist(PersistenceEntitlements(name: name, path: path))
            }
        }

        // Add to configuration
        config = AppConfig(
            appId: config.appId,
            version: config.version,
            entitlements: config.entitlements + [newEntitlement]
        )

        // Save configuration
        try saveConfig(config, to: wendyJsonPath)

        Noora().success("Added \(newEntitlement.type.rawValue) entitlement")
        if let mode {
            print("   Mode: \(mode)")
        }
    }

    private func createEntitlement(type: EntitlementType, mode: String?) throws -> Entitlement {
        switch type {
        case .network:
            let networkMode: NetworkMode
            if let modeString = mode {
                guard let parsedMode = NetworkMode(rawValue: modeString) else {
                    throw CLIError.invalidArgument(name: "mode", value: modeString, reason: "Invalid for entitlement type '\(type.rawValue)'")
                }
                networkMode = parsedMode
            } else {
                networkMode = .host  // Default
            }
            return .network(NetworkEntitlements(mode: networkMode))

        case .bluetooth:
            let bluetoothMode: BluetoothEntitlements.BluetoothMode
            if let modeString = mode {
                guard let parsedMode = BluetoothEntitlements.BluetoothMode(rawValue: modeString)
                else {
                    throw CLIError.invalidArgument(name: "mode", value: modeString, reason: "Invalid for entitlement type '\(type.rawValue)'")
                }
                bluetoothMode = parsedMode
            } else {
                bluetoothMode = .kernel  // Default
            }
            return .bluetooth(BluetoothEntitlements(mode: bluetoothMode))

        case .video:
            return .video(VideoEntitlements())

        case .audio:
            return .audio

        case .gpu:
            return .gpu(GPUEntitlements())

        case .persist:
            guard let name, let path else {
                throw CLIError.missingArgument(name: "name/path", description: "--name and --path are required for persist entitlement")
            }

            return .persist(PersistenceEntitlements(name: name, path: path))
        }
    }
}

// MARK: - Remove Command

struct RemoveCommand: ModifyProjectCommand {
    static let configuration = CommandConfiguration(
        commandName: "remove",
        abstract: "Remove an entitlement from the project"
    )

    @Option(help: "Type of entitlement to remove (network, bluetooth, video)")
    var entitlementType: EntitlementType?

    @Option(
        help: "Path to the project directory (defaults to current directory)"
    )
    var project: String = "."

    private var logger: Logger {
        Logger(label: "sh.wendy.cli.project.entitlements.remove")
    }

    func run() async throws {
        let wendyJsonPath = getWendyJsonPath()

        // Check if wendy.json exists
        guard FileManager.default.fileExists(atPath: wendyJsonPath) else {
            print("❌ No wendy.json found in \(project)")
            print("Run 'wendy project init' to initialize a new project")
            throw CLIError.configNotFound(path: wendyJsonPath)
        }

        // Load current configuration
        var config = try loadConfig(from: wendyJsonPath)
        let removedEntitlementType: EntitlementType

        if let entitlementType {
            // Check if entitlement exists
            guard config.entitlements.contains(where: { $0.type == entitlementType }) else {
                Noora().warning("\(entitlementType.rawValue.capitalized) entitlement not found")
                return
            }

            removedEntitlementType = entitlementType
        } else {
            Noora().info("Select an entitlement to remove")

            let index = try await Noora().selectableTable(
                headers: [
                    .primary("Entitlement")
                ],
                rows: config.entitlements.map { entitlement in
                    return [
                        .plain(entitlement.type.rawValue.capitalized)
                    ]
                },
                pageSize: config.entitlements.count
            )

            removedEntitlementType = config.entitlements[index].type
        }

        // Remove entitlement
        config = AppConfig(
            appId: config.appId,
            version: config.version,
            entitlements: config.entitlements.filter { $0.type != removedEntitlementType }
        )

        // Save configuration
        try saveConfig(config, to: wendyJsonPath)

        Noora().success("Removed \(removedEntitlementType.rawValue) entitlement")
    }
}

// MARK: - Extensions

extension Entitlement {
    var type: EntitlementType {
        switch self {
        case .network:
            return .network
        case .bluetooth:
            return .bluetooth
        case .video:
            return .video
        case .audio:
            return .audio
        case .gpu:
            return .gpu
        case .persist:
            return .persist
        }
    }
}

// MARK: - Errors

