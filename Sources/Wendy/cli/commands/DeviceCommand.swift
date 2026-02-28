import ArgumentParser
import CLIOutput
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOFoundationCompat
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
                    AppsCommand.self,
                ]
            ),
            CommandGroup(
                name: "Connectivity",
                subcommands: [
                    WiFiCommand.self,
                    BluetoothCommand.self,
                ]
            ),
            CommandGroup(
                name: "Debug Tools",
                subcommands: [
                    LogsCommand.self,
                    DashboardCommand.self,
                    AudioCommand.self,
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

        @Flag(help: "Check for pre-releases")
        var prerelease: Bool = false

        struct JSONOutput: Codable {
            let currentVersion: String
            let os: String?
            let osVersion: String?
            let cpuArchitecture: String?
            let featureset: Set<String>
            let latestVersion: String?
        }

        @OptionGroup var target: TargetOptions

        func run() async throws {
            let version = try await withAgentGRPCClient(
                target,
                title: "For which device do you want to get the agent version?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                return try await agent.getAgentVersion(request: .init(message: .init()))
            }

            var latestVersion: String? = nil

            if let releases = try? await fetchReleases(timeout: .seconds(5)) {
                if prerelease {
                    latestVersion = releases.first?.name
                } else {
                    latestVersion = releases.first(where: { $0.prerelease == false })?.name
                }
            }

            if JSONMode.isEnabled {
                let output = JSONOutput(
                    currentVersion: version.version,
                    os: version.os,
                    osVersion: version.hasOsVersion ? version.osVersion : nil,
                    cpuArchitecture: version.cpuArchitecture,
                    featureset: Set(version.featureset),
                    latestVersion: latestVersion
                )
                let data = try JSONEncoder().encode(output)
                print(String(data: data, encoding: .utf8)!)
            } else {
                print("Agent version: \(version.version)")
                if !version.os.isEmpty {
                    print("OS: \(version.os)")
                }
                if version.hasOsVersion {
                    print("OS version: \(version.osVersion)")
                }
                if !version.cpuArchitecture.isEmpty {
                    print("Architecture: \(version.cpuArchitecture)")
                }
                if !version.featureset.isEmpty {
                    print("Features: \(version.featureset.joined(separator: ", "))")
                }
                if let latestVersion, version.version != latestVersion {
                    print("Update available: \(latestVersion)")
                } else if latestVersion != nil {
                    print("Up to date")
                }
            }
        }
    }

    struct SetDefaultCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "set-default",
            abstract: "Set the default device."
        )

        @OptionGroup var target: TargetOptions

        func run() async throws {
            let endpoint = try await target.read(
                title: "Set default device",
                readDefault: false
            )

            var config = getConfig()
            switch endpoint {
            case .lan(let host, _, _):
                config.defaultDevice = host
            case .bluetooth, .external:
                ()
            }
            try config.save()

            cliOutput.success(
                "Default device set to \(config.defaultDevice ?? "none")"
            )
        }
    }

    struct UnsetDefaultCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "unset-default",
            abstract: "Unset the default device."
        )

        @OptionGroup var target: TargetOptions

        func run() async throws {
            var config = getConfig()
            config.defaultDevice = nil
            try config.save()

            cliOutput.success("Default device unset")
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

        @OptionGroup var target: TargetOptions

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
                        // Query the device for its architecture
                        targetPlatform = try await withAgentGRPCClient(
                            agentConnectionOptions,
                            title: "Detecting device architecture"
                        ) { client in
                            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                            let version = try await agent.getAgentVersion(request: .init(message: .init()))
                            return try Platform.linuxPlatform(forArchitecture: version.cpuArchitecture)
                        }
                    }

                    binary = try await downloadLatestRelease(
                        platform: targetPlatform,
                        includePrerelease: prerelease
                    ).path
                }

                let endpoint = try await withAgentGRPCClientAndEndpoint(
                    target,
                    title: "Which device do you want to update?"
                ) { client, endpoint in
                    let agent = Agent(client: client)
                    _ = try await cliOutput.withProgressBar(
                        message: "Updating Device",
                        successMessage: "Device updated",
                        errorMessage: "Device update failed"
                    ) {
                        updateProgress in
                        try await agent.update(fromBinary: binary, onProgress: updateProgress)
                    }
                    return endpoint
                }

                // Wait for the gRPC socket to come back up after the device restarts
                guard case .grpc(let host, let port) = endpoint.remote else {
                    throw CLIError.invalidEndpoint("Cannot wait for restart on non-gRPC endpoint")
                }
                try await waitForDeviceRestart(
                    host: host,
                    port: port
                )

                try await waitForDeviceRestart(endpoint: endpoint)
            #endif
        }
    }

    struct SetupCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "setup",
            abstract: "Setup the Wendy agent."
        )

        @OptionGroup var target: TargetOptions

        func run() async throws {
            try await withCloudGRPCClient(title: "Setup agent") { cloudClient in
                let orgs = try await cloudClient.listOrganizations()

                if orgs.isEmpty {
                    cliOutput.error("No organizations found")
                    Self.exit(withError: nil)
                }

                let orgName = try await cliOutput.singleChoicePrompt(
                    title: "Enroll device",
                    question: "Which organization do you want to enroll into?",
                    options: orgs.map(\.description)
                )
                let org = orgs.first(where: { $0.description == orgName })!

                let name = try await cliOutput.textPrompt(
                    title: "Name your device",
                    prompt: "Name"
                )

                let certsAPI = Wendycloud_V1_CertificateService.Client(wrapping: cloudClient.grpc)
                let tokenResponse = try await certsAPI.createAssetEnrollmentToken(
                    .with {
                        $0.organizationID = org.id
                        $0.name = name
                    },
                    metadata: cloudClient.metadata
                )

                try await withAgentClient(target, title: "Provisioning device") {
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
                            target,
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
                target,
                title: "Listing available WiFi networks"
            ) { agent, hostname -> Bool in
                let setupWifi = try await cliOutput.yesOrNoPrompt(
                    question: "Do you want to setup WiFi?",
                    defaultAnswer: true
                )

                if setupWifi {
                    while !Task.isCancelled {
                        let ssid = try await agent.discoverSSID()

                        let password = try cliOutput.secureTextPrompt(
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
                    let shouldUpdate = try await cliOutput.yesOrNoPrompt(
                        question: "Do you want to update the agent?",
                        defaultAnswer: true
                    )

                    guard shouldUpdate, case .grpc(let client) = agent else {
                        return false
                    }

                    // Detect platform of remote device
                    let agentService = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    let versionInfo = try await agentService.getAgentVersion(request: .init(message: .init()))
                    let devicePlatform = try Platform.linuxPlatform(forArchitecture: versionInfo.cpuArchitecture)
                    let binary = try await downloadLatestRelease(platform: devicePlatform).path
                    _ = try await cliOutput.withProgressBar(
                        message: "Updating Device",
                        successMessage: "Device updated",
                        errorMessage: "Device update failed"
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
                let endpoint = try await target.read(
                    title: "Waiting for device",
                    includeBluetooth: false
                )

                if case .lan(let host, let port, _) = endpoint {
                    try await waitForDeviceRestart(
                        host: host,
                        port: port
                    )
                }
            }
        }
    }
}

extension Wendycloud_V1_Organization: CustomStringConvertible {
    public var description: String {
        self.name
    }
}
