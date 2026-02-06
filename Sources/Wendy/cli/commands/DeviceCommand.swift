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
                name: "Observability",
                subcommands: [
                    LogsCommand.self,
                    DashboardCommand.self,
                    TelemetryStreamCommand.self,
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
            let osVersion: String?
            let latestVersion: String?
        }

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let version = try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "For which device do you want to get the agent version?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                return try await agent.getAgentVersion(request: .init(message: .init()))
            }

            var latestVersion: String? = nil

            if checkUpdates, let releases = try? await fetchReleases() {
                if prerelease {
                    latestVersion = releases.first?.name
                } else {
                    latestVersion = releases.first(where: { $0.prerelease == false })?.name
                }
            }

            if JSONMode.isEnabled {
                let output = JSONOutput(
                    currentVersion: version.version,
                    osVersion: version.hasOsVersion ? version.osVersion : nil,
                    latestVersion: latestVersion
                )
                let data = try JSONEncoder().encode(output)
                print(String(data: data, encoding: .utf8)!)
            } else {
                print("Agent version: \(version.version)")
                if version.hasOsVersion {
                    print("OS version: \(version.osVersion)")
                }
                if let latestVersion, version.version != latestVersion {
                    print("Update available: \(latestVersion)")
                } else if checkUpdates {
                    print("No update available")
                }
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
            switch endpoint {
            case .lan(let host, _, _):
                config.defaultDevice = host
            case .bluetooth:
                ()
            case .localDocker:
                ()
            }
            try config.save()

            Noora(theme: .emerald()).success(
                "Default device set to \(config.defaultDevice ?? "none")"
            )
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

            Noora(theme: .emerald()).success("Default device unset")
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
            #if os(Windows)
                cliOutput.error("Device update is not supported on Windows hosts")
            #else
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
                            throw CLIError.invalidArgument(
                                name: "platform",
                                value: platformStr,
                                reason: "Use 'linux-aarch64' or 'linux-x86_64'"
                            )
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

                let endpoint = try await withAgentGRPCClientAndEndpoint(
                    agentConnectionOptions,
                    title: "Which device do you want to update?"
                ) { client, endpoint in
                    let agent = Agent(client: client)
                    _ = try await Noora(theme: .emerald()).progressBarStep(
                        message: "Updating Device"
                    ) {
                        updateProgress in
                        try await agent.update(fromBinary: binary, onProgress: updateProgress)
                    }
                    return endpoint
                }

                // Wait for the gRPC socket to come back up after the device restarts
                try await waitForDeviceRestart(endpoint: endpoint)

                cliOutput.success("Agent updated successfully")
            #endif
        }
    }

    struct SetupCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "setup",
            abstract: "Setup the Wendy agent."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withCloudGRPCClient(title: "Setup agent") { cloudClient in
                let orgs = try await cloudClient.listOrganizations()

                if orgs.isEmpty {
                    Noora(theme: .emerald()).error("No organizations found")
                    Self.exit(withError: nil)
                }

                let org = Noora(theme: .emerald()).singleChoicePrompt(
                    title: "Enroll device",
                    question: "Which organization do you want to enroll into?",
                    options: orgs
                )

                let name = Noora(theme: .emerald()).textPrompt(
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

                try await withAgentClient(agentConnectionOptions, title: "Provisioning device") {
                    agent in
                    switch agent {
                    case .grpc(let client):
                        let agent = Agent(client: client)
                        try await agent.provision(
                            enrollmentToken: tokenResponse.enrollmentToken,
                            assetID: tokenResponse.assetID,
                            organizationID: org.id,
                            cloudHost: cloudClient.cloudHost
                        )
                    case .bluetooth:
                        // TODO: Implement Bluetooth provisioning
                        throw CancellationError()
                    }
                }
            }

            func getWifiStatus() async throws -> WiFiStatusInfo {
                while true {
                    try Task.checkCancellation()
                    do {
                        return try await withAgentClient(
                            agentConnectionOptions,
                            title: "Checking agent status"
                        ) { agent in
                            try await agent.getWiFiStatus()
                        }
                    } catch {
                        continue
                    }
                }
            }

            let shouldWaitForRestart = try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Listing available WiFi networks"
            ) { agent, hostname -> Bool in
                let setupWifi = Noora(theme: .emerald()).yesOrNoChoicePrompt(
                    question: "Do you want to setup WiFi?",
                    collapseOnSelection: false
                )

                if setupWifi {
                    while !Task.isCancelled {
                        let ssid = try await agent.discoverSSID()

                        let password = try secureTextPrompt(
                            title: "Enter the password for '\(ssid)'",
                            prompt: "Password"
                        )

                        let result = try await agent.connectToWiFi(
                            ssid: ssid,
                            password: password
                        )

                        if result.success {
                            cliOutput.success("Connected to WiFi network \(ssid)")
                            break
                        } else {
                            cliOutput.error(
                                "Failed to connect to WiFi network: \(result.errorMessage ?? "Unknown error")"
                            )
                        }
                    }
                }

                #if !os(Windows)
                    let shouldUpdate = Noora(theme: .emerald()).yesOrNoChoicePrompt(
                        question: "Do you want to update the agent?",
                        collapseOnSelection: false
                    )

                    guard shouldUpdate, case .grpc(let client) = agent else {
                        return false
                    }

                    // TODO: Detect platform of remote device
                    // Default to Linux aarch64 for device updates during setup
                    let binary = try await downloadLatestRelease(platform: .linuxAarch64).path
                    _ = try await Noora(theme: .emerald()).progressBarStep(
                        message: "Updating Device"
                    ) {
                        updateProgress in
                        try await Agent(client: client).update(
                            fromBinary: binary,
                            onProgress: updateProgress
                        )
                    }

                    return true
                #else
                    return false
                #endif
            }

            if shouldWaitForRestart {
                // Get the endpoint to wait for device restart
                // The device is now provisioned, so this will connect via mTLS on the proper port
                let endpoint = try await agentConnectionOptions.read(
                    title: "Waiting for device",
                    includeBluetooth: false
                )

                if case .lan(let host, let port, let defaultDevice) = endpoint {
                    try await waitForDeviceRestart(
                        endpoint: AgentConnectionOptions.Endpoint(
                            host: host,
                            port: port,
                            defaultDevice: defaultDevice
                        )
                    )
                }

                cliOutput.success("Agent updated successfully")
            }
        }
    }
}

extension Wendycloud_V1_Organization: CustomStringConvertible {
    public var description: String {
        self.name
    }
}
