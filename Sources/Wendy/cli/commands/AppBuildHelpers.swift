import Analytics
import AppConfig
import ArgumentParser
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Noora
import WendyAgentGRPC

#if os(macOS)
    import AppKit
#endif

private struct SendableProgressUpdater: @unchecked Sendable {
    let call: (Double) -> Void

    init(_ call: @escaping (Double) -> Void) {
        self.call = call
    }

    @MainActor
    func update(_ value: Double) {
        self.call(value)
    }
}

private actor UnpackProgressTracker {
    private var totalBytes: Int64?
    private var totalLayers: Int = 0
    private var completedBytes: Int64 = 0
    private var seenLayerIndices: Set<Int> = []
    private var layerSizes: [Int: Int64] = [:]

    func progressValue(
        for update: Wendy_Agent_Services_V1_CreateContainerProgress
    ) -> Double? {
        switch update.phase {
        case .unpacking:
            if update.totalLayers > 0 {
                totalLayers = Int(update.totalLayers)
            }
            if update.layerSize > 0 {
                if totalBytes == nil {
                    totalBytes = update.layerSize
                    recomputeCompletedBytes()
                } else {
                    totalBytes = update.layerSize
                }
            }
            return 0
        case .applyingLayer:
            if totalLayers == 0 && update.totalLayers > 0 {
                totalLayers = Int(update.totalLayers)
            }
            let index = Int(update.layerIndex)
            if index > 0 {
                if update.layerSize > 0 {
                    layerSizes[index] = update.layerSize
                }
                if !seenLayerIndices.contains(index) {
                    seenLayerIndices.insert(index)
                    if totalBytes != nil {
                        completedBytes += layerSizes[index] ?? 0
                    }
                }
            }

            if let totalBytes, totalBytes > 0 {
                let value = Double(completedBytes) / Double(totalBytes)
                return min(max(value, 0), 1)
            }

            if totalLayers > 0 {
                let value = Double(update.layerIndex) / Double(totalLayers)
                return min(max(value, 0), 1)
            }

            return nil
        case .creatingContainer, .complete:
            return 1
        case .unspecified, .UNRECOGNIZED:
            return nil
        }
    }

    private func recomputeCompletedBytes() {
        completedBytes = seenLayerIndices.reduce(Int64(0)) { total, index in
            total + (layerSizes[index] ?? 0)
        }
    }
}

/// Shared helper methods for building and preparing apps.
/// Used by both RunCommand and BuildCommand to avoid code duplication.
enum AppBuildHelpers {
    enum Error: Swift.Error, CustomStringConvertible {
        case failedToUploadLayers(Int)
        case noExecutableTarget
        case invalidExecutableTarget(String)
        case multipleExecutableTargets([String])
        case noManifestFound

        var description: String {
            switch self {
            case .failedToUploadLayers:
                return "Failed to upload"
            case .noExecutableTarget:
                return "No executable target found in package"
            case .invalidExecutableTarget(let name):
                return "No executable target named '\(name)' found in package"
            case .multipleExecutableTargets(let names):
                return
                    "multiple executable targets available, but none specified: \(names.joined(separator: ", "))"
            case .noManifestFound:
                return "No manifest found in Docker image"
            }
        }
    }

    /// Check if Docker is running and optionally prompt the user to start it
    static func checkDockerIsRunning(shouldAutoAccept: Bool) async throws {
        let docker = DockerCLI()

        while true {
            do {
                _ = try await docker.getServerVersion()
                return
            } catch {
                // In JSON mode, just fail with an error - cannot prompt to start Docker
                if JSONMode.isEnabled {
                    JSONErrorResponse(
                        error: "docker_not_running",
                        reason: "Docker is not running",
                        suggestion:
                            "Please start Docker Desktop or OrbStack before running this command"
                    ).print()
                    throw ExitCode.failure
                }

                cliOutput.warning("Docker is not running")

                #if os(macOS)
                    // Check if Docker.app, OrbStack.app is installed
                    if let url = NSWorkspace.shared.urlForApplication(
                        withBundleIdentifier: "com.docker.docker"
                    ) {
                        Noora().info("Docker Desktop is installed")

                        guard
                            Noora().yesOrNoChoicePrompt(
                                question: "Do you want to open Docker Desktop?"
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(url) {
                            Noora().info("Opening Docker.app")
                            while true {
                                do {
                                    _ = try await docker.getServerVersion()
                                    return
                                } catch {
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                            }
                        } else {
                            Noora().info(
                                "Failed to open Docker Desktop automatically, please open it manually"
                            )
                        }
                        return
                    } else if let url = NSWorkspace.shared.urlForApplication(
                        withBundleIdentifier: "com.orbstack.orbstack"
                    ) {
                        Noora().info("OrbStack.app is installed")

                        guard
                            Noora().yesOrNoChoicePrompt(
                                question: "Do you want to open OrbStack?"
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(url) {
                            Noora().info("Opening OrbStack")
                            while true {
                                do {
                                    _ = try await docker.getServerVersion()
                                    return
                                } catch {
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                            }
                        } else {
                            Noora().info(
                                "Failed to open OrbStack automatically, please open it manually"
                            )
                        }
                        return
                    } else {
                        cliOutput.warning("Docker.app or OrbStack.app is not installed")
                        guard
                            Noora().yesOrNoChoicePrompt(
                                question: "Do you want to open the installation guide?"
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(
                            URL(string: "https://docs.docker.com/get-docker/")!
                        ) {
                            Noora().info("Opening Docker documentation")
                        } else {
                            Noora().error("Failed to open Docker documentation")
                        }
                    }
                #endif
            }
        }
    }

    /// Check Swift requirements and install SDK/toolchain if needed
    static func checkSwiftRequirements(
        swiftVersion: String,
        swiftSDK: String,
        sdkDownloadURL: String,
        sdkChecksum: String,
        shouldAutoAccept: Bool
    ) async throws {
        // First, check if swiftly is available
        let swiftlyAvailable = await SwiftPM.isSwiftlyAvailable()
        guard swiftlyAvailable else {
            if JSONMode.isEnabled {
                JSONErrorResponse(
                    error: "swiftly_not_installed",
                    reason: "Swiftly is not installed on your system",
                    suggestion:
                        "Install swiftly by running: curl -L https://swiftlang.github.io/swiftly/swiftly-install.sh | bash"
                ).print()
            } else {
                Noora().error(
                    """
                    Swiftly is not installed on your system.

                    Swiftly is required to manage Swift toolchains for WendyOS development.

                    To install Swiftly, run:
                        curl -L https://swiftlang.github.io/swiftly/swiftly-install.sh | bash

                    After installation, restart your terminal or run:
                        source ~/.local/share/swiftly/env.sh

                    For more information, visit: https://github.com/swiftlang/swiftly
                    """
                )
            }
            throw ExitCode.failure
        }

        let swiftPM = SwiftPM()

        // Check with spinner
        let (installedSDKs, installedSwiftVersions) = try await cliOutput.withProgress(
            message: "Checking Swift requirements",
            successMessage: "Swift environment ready",
            errorMessage: "Failed to check Swift requirements"
        ) {
            async let sdks = try await swiftPM.listSDKs()
            async let versions = try await swiftPM.listSwiftVersions()
            return try await (sdks, versions)
        }

        if !installedSDKs.contains(swiftSDK) {
            let installSDK: Bool

            if shouldAutoAccept {
                installSDK = true
            } else {
                installSDK = Noora().yesOrNoChoicePrompt(
                    question: "Do you want to install/update the WendyOS Swift SDK?"
                )
            }

            if installSDK {
                try await cliOutput.withProgress(
                    message: "Installing WendyOS Swift SDK",
                    successMessage: "WendyOS SDK ready to use",
                    errorMessage: "Failed to install SDK"
                ) {
                    try await swiftPM.installSDK(
                        from: sdkDownloadURL,
                        checksum: sdkChecksum
                    )
                }
            }
        }

        if !installedSwiftVersions.contains(where: { $0.version.name == swiftVersion }) {
            let installSwift: Bool

            if shouldAutoAccept {
                installSwift = true
            } else {
                installSwift = Noora().yesOrNoChoicePrompt(
                    title: "Swift \(swiftVersion) version is not installed yet",
                    question: "Do you want to install Swift \(swiftVersion)?",
                    description: """
                        WendyOS development is tied to a specific Swift toolchain.
                        We update this version from time to time to ensure compatibility with the latest features.
                        """
                )
            }

            if installSwift {
                try await cliOutput.withStreamingOutput(
                    title: "Installing Swift \(swiftVersion)",
                    maxLines: 15
                ) { emit in
                    try await swiftPM.installSDK(
                        from: sdkDownloadURL,
                        checksum: sdkChecksum
                    )
                }
                cliOutput.success("Swift \(swiftVersion) Installed")
            }
        }
    }

    /// Create a container on the agent
    static func createContainerdContainer(
        appName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        restartPolicy: RestartPolicy,
        progress: ((Double) -> Void)? = nil
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.build.containerd.create")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        let appConfigData = try await readAppConfigData(logger: logger)
        let request = Wendy_Agent_Services_V1_CreateContainerRequest.with {
            // The image is pushed to the device's local registry as just "appName"
            // The host.docker.internal:port prefix is only for routing during push
            $0.imageName = "\(appName):latest"
            $0.appName = appName
            $0.appConfig = appConfigData
            $0.restartPolicy = restartPolicy
        }

        let progressHandler = SendableProgressUpdater(progress ?? { _ in })
        let progressTracker = UnpackProgressTracker()

        try await agentContainers.createContainerWithProgress(request) { response in
            switch response.accepted {
            case .success(let contents):
                for try await bodyPart in contents.bodyParts {
                    switch bodyPart {
                    case .message(let message):
                        switch message.responseType {
                        case .progress(let progressUpdate):
                            if let value = await progressTracker.progressValue(
                                for: progressUpdate
                            ) {
                                await progressHandler.update(value)
                            }
                        case .completed:
                            await progressHandler.update(1)
                        case .none:
                            break
                        }
                    case .trailingMetadata:
                        break
                    }
                }
            case .failure(let error):
                throw error
            }
        }
    }

    /// Read app configuration from wendy.json if present
    static func readAppConfigData(logger: Logger) async throws -> Data {
        do {
            let appConfigData = try Data(contentsOf: URL(fileURLWithPath: "./wendy.json"))
            // Validate data
            _ = try JSONDecoder().decode(AppConfig.self, from: appConfigData)
            return appConfigData
        } catch {
            logger.debug("Failed to decode app config", metadata: ["error": .string("\(error)")])
            Noora().info("No valid wendy.json was found. Using default settings.")
            return Data()
        }
    }

    // MARK: - Phase Analytics

    /// Track a phase failure for analytics
    static func trackPhaseFailure(
        phase: String,
        runtime: String,
        commandName: String,
        error: Swift.Error,
        additionalProperties: [String: String] = [:]
    ) async {
        guard let analytics = AnalyticsService.current else { return }
        let sanitizedError = ErrorSanitizer.sanitize(error)
        var properties: [String: String] = [
            "phase": phase,
            "runtime": runtime,
            "command_name": commandName,
            "error_type": sanitizedError.type,
            "error_name": sanitizedError.name,
            "error_domain": sanitizedError.domain,
        ]
        for (key, value) in additionalProperties {
            properties[key] = value
        }
        await analytics.trackEvent(
            name: "run_phase_failed",
            properties: properties
        )
    }

    /// Execute a phase with failure tracking
    static func executePhase<T: Sendable>(
        phase: String,
        runtime: String,
        commandName: String,
        additionalProperties: [String: String] = [:],
        operation: @Sendable () async throws -> T
    ) async throws -> T {
        do {
            return try await operation()
        } catch {
            await trackPhaseFailure(
                phase: phase,
                runtime: runtime,
                commandName: commandName,
                error: error,
                additionalProperties: additionalProperties
            )
            throw error
        }
    }
}
