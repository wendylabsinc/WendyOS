import ArgumentParser
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOFoundationCompat
import Noora
import WendyAgentGRPC
import WendyCloudGRPC
import WendySDK
import X509
import _NIOFileSystem

#if os(macOS) || os(iOS) || os(tvOS) || os(watchOS)
    import Darwin
#elseif os(Linux)
    import Glibc
#endif

/// Prompt for password input without echoing to terminal
private func securePasswordPrompt(_ prompt: String) -> String {
    guard let password = getpass(prompt) else {
        return ""
    }
    return String(cString: password)
}

struct DeviceCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "device",
        abstract: "Control your Wendy device.",
        subcommands: [
            SetDefaultCommand.self,
            UnsetDefaultCommand.self,
        ],
        groupedSubcommands: [
            CommandGroup(
                name: "Device Management",
                subcommands: [
                    SetupCommand.self,
                    HardwareCommand.self,
                    WiFiCommand.self,
                    AppsCommand.self,
                ]
            ),
            CommandGroup(
                name: "Debugging",
                subcommands: [
                    HardwareCommand.self
                ]
            ),
            CommandGroup(
                name: "Update",
                subcommands: [
                    VersionCommand.self,
                    UpdateCommand.self,
                ]
            ),
        ]
    )

    struct VersionCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "version",
            abstract: "Get the version of the Wendy agent."
        )

        @Flag(help: "Check for updates")
        var checkUpdates: Bool = false

        @Flag(help: "Check for pre-releases")
        var prerelease: Bool = false

        struct JSONOutput: Codable {
            let currentVersion: String
            let latestVersion: String?
        }

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let version: String = try await withAgentConnection(
                agentConnectionOptions,
                title: "For which device do you want to get the agent version?",
                grpcOperation: { client in
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    let response = try await agent.getAgentVersion(request: .init(message: .init()))
                    return response.version
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                        let response = try await executeBluetoothCommand(
                            .agentVersion,
                            deviceIdentifier: deviceIdentifier
                        )
                        if case .agentVersion(let version) = response {
                            return version
                        } else if case .error(let message) = response {
                            throw VersionCommandError.operationFailed(message)
                        }
                        throw VersionCommandError.operationFailed("Unexpected response")
                    #else
                        throw BluetoothNotAvailableError()
                    #endif
                }
            )

            var latestVersion: String? = nil

            if checkUpdates, let releases = try? await fetchReleases() {
                if prerelease {
                    latestVersion = releases.first?.name
                } else {
                    latestVersion = releases.first(where: { $0.prerelease == false })?.name
                }
            }

            if JSONMode.isEnabled {
                let output = JSONOutput(currentVersion: version, latestVersion: latestVersion)
                let data = try JSONEncoder().encode(output)
                print(String(data: data, encoding: .utf8)!)
            } else {
                Noora().info("Current version: \(version)")
                if let latestVersion, version != latestVersion {
                    Noora().warning("Update available: \(latestVersion)")
                } else if checkUpdates {
                    Noora().success("No update available")
                }
            }
        }
    }

    enum VersionCommandError: Error, LocalizedError {
        case operationFailed(String)

        var errorDescription: String? {
            switch self {
            case .operationFailed(let message):
                return message
            }
        }
    }

    struct SetDefaultCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "set-default",
            abstract: "Set the default device."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let endpoint = try await agentConnectionOptions.read(
                title: "Set default device",
                readDefault: false
            )

            var config = getConfig()
            config.defaultDevice = endpoint.host
            try config.save()

            Noora().success("Default device set to \(endpoint.host)")
        }
    }

    struct UnsetDefaultCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "unset-default",
            abstract: "Unset the default device."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            var config = getConfig()
            config.defaultDevice = nil
            try config.save()

            Noora().success("Default device unset")
        }
    }

    struct UpdateCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "update",
            abstract: "Update the Wendy agent."
        )

        @Option(help: "The path to the new version of the Wendy agent.")
        var binary: String?

        @Option(
            help:
                "Target platform for the agent binary (linux-aarch64 or linux-x86_64). Defaults to linux-aarch64."
        )
        var platform: String?

        @Flag(help: "Download the latest pre-release version instead of stable release")
        var prerelease: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let binary: String

            if let location = self.binary {
                binary = location
            } else {
                // Determine target platform (devices are always Linux)
                let targetPlatform: Platform
                if let platformStr = platform {
                    switch platformStr.lowercased() {
                    case "linux-aarch64", "aarch64", "arm64":
                        targetPlatform = .linuxAarch64
                    case "linux-x86_64", "x86_64", "amd64":
                        targetPlatform = .linuxX86_64
                    default:
                        Noora().error(
                            "Invalid platform '\(platformStr)'. Use 'linux-aarch64' or 'linux-x86_64'"
                        )
                        Self.exit(withError: nil)
                    }
                } else {
                    // Default to aarch64 (most common for devices)
                    targetPlatform = .linuxAarch64
                }

                binary = try await downloadLatestRelease(
                    platform: targetPlatform,
                    includePrerelease: prerelease
                ).path
            }

            let success = try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to update?"
            ) { client in
                let agent = Agent(client: client)
                return try await Noora().progressBarStep(message: "Updating Device") {
                    updateProgress in
                    try await agent.update(fromBinary: binary, onProgress: updateProgress)
                }
            }

            guard success else {
                Noora().error("Failed to update agent")
                Self.exit(withError: nil)
            }

            Noora().success("Agent updated successfully")
        }
    }

    struct SetupCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "setup",
            abstract: "Setup the Wendy agent."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let endpoint = try await withCloudGRPCClient(title: "Setup agent") { cloudClient in
                let orgs = try await cloudClient.listOrganizations()

                if orgs.isEmpty {
                    Noora().error("No organizations found")
                    Self.exit(withError: nil)
                }

                let org = Noora().singleChoicePrompt(
                    title: "Enroll device",
                    question: "Which organization do you want to enroll into?",
                    options: orgs
                )

                let name = Noora().textPrompt(
                    title: "Name your device",
                    prompt: "Name",
                    collapseOnAnswer: false
                )

                let certsAPI = Wendycloud_V1_CertificateService.Client(wrapping: cloudClient.grpc)
                let tokenResponse = try await certsAPI.createAssetEnrollmentToken(
                    .with {
                        $0.organizationID = org.id
                        $0.name = name
                    },
                    metadata: cloudClient.metadata
                )

                let endpoint = try await agentConnectionOptions.read(title: "Provisioning device")
                try await withAgentGRPCClient(endpoint, title: "Provisioning device") { client in
                    let agent = Agent(client: client)
                    try await agent.provision(
                        enrollmentToken: tokenResponse.enrollmentToken,
                        assetID: tokenResponse.assetID,
                        organizationID: org.id,
                        cloudHost: endpoint.host
                    )
                }
                return endpoint
            }

            func getWifiStatus() async throws -> Wendy_Agent_Services_V1_GetWiFiStatusResponse {
                while !Task.isCancelled {
                    do {
                        return try await withAgentGRPCClient(
                            endpoint,
                            title: "Checking agent status"
                        ) { client in
                            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(
                                wrapping: client
                            )
                            let response = try await agent.getAgentVersion(.init())
                            Noora().info("Agent is provisioned (version: \(response.version))")
                            return try await agent.getWiFiStatus(.init())
                        }
                    } catch {
                        continue  // Failed to check agent status, try again
                    }
                }

                throw CancellationError()
            }

            let status = try await getWifiStatus()

            try await withAgentGRPCClient(
                endpoint,
                title: "Listing available WiFi networks"
            ) { client in
                let agent = Agent(client: client)

                if !status.connected {
                    let setupWifi = Noora().yesOrNoChoicePrompt(
                        question: "Do you want to setup WiFi?",
                        collapseOnSelection: false
                    )

                    if setupWifi {
                        while !Task.isCancelled {
                            let ssid = try await agent.discoverSSID()

                            let password = securePasswordPrompt("Password for '\(ssid)': ")

                            let result = try await agent.connectToWiFi(
                                ssid: ssid,
                                password: password
                            )

                            if result.success {
                                Noora().success("Connected to WiFi network \(ssid)")
                                break
                            } else {
                                Noora().error(
                                    "Failed to connect to WiFi network: \(result.errorMessage)"
                                )
                            }
                        }
                    }
                }

                let shouldUpdate = Noora().yesOrNoChoicePrompt(
                    question: "Do you want to update the agent?",
                    collapseOnSelection: false
                )

                guard shouldUpdate else {
                    return
                }

                // TODO: Detect platform of remote device
                // Default to Linux aarch64 for device updates during setup
                let binary = try await downloadLatestRelease(platform: .linuxAarch64).path
                let success = try await Noora().progressBarStep(message: "Updating Device") {
                    updateProgress in
                    try await agent.update(fromBinary: binary, onProgress: updateProgress)
                }

                guard success else {
                    Noora().error("Failed to update agent")
                    Self.exit(withError: nil)
                }

                Noora().success("Agent updated successfully")
            }
        }
    }
}

extension Wendycloud_V1_Organization: CustomStringConvertible {
    public var description: String {
        self.name
    }
}
