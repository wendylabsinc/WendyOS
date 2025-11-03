import AppConfig
import ArgumentParser
import Foundation
import Logging
import Noora
import SystemPackage

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
            print("❌ No wendy.json found in current directory")
            print("Run 'wendy project init' to initialize a new project")
            throw ProjectError.configNotFound(path: wendyJsonPath)
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
        case .video:
            print("   No additional configuration")
        case .audio:
            print("   No additional configuration")
        case .gpu:
            print("   No additional configuration")
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
            throw ProjectError.configNotFound(path: wendyJsonPath)
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
            let availableEntitlementTypes = EntitlementType.allCases.filter { entitlement in
                !config.entitlements.contains { $0.type == entitlement }
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
                newEntitlement = .video(VideoEntitlements())
            case .audio:
                newEntitlement = .audio
            case .gpu:
                newEntitlement = .gpu(GPUEntitlements())
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
                    throw ProjectError.invalidMode(mode: modeString, for: type)
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
                    throw ProjectError.invalidMode(mode: modeString, for: type)
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
            throw ProjectError.configNotFound(path: wendyJsonPath)
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
        }
    }
}

// MARK: - Errors

enum ProjectError: Error {
    case configNotFound(path: String)
    case invalidMode(mode: String, for: EntitlementType)
    case saveFailed(path: String, error: String)

    var localizedDescription: String {
        switch self {
        case .configNotFound(let path):
            return "Configuration file not found at '\(path)'"
        case .invalidMode(let mode, let type):
            return "Invalid mode '\(mode)' for entitlement type '\(type.rawValue)'"
        case .saveFailed(let path, let error):
            return "Failed to save configuration to '\(path)': \(error)"
        }
    }
}
