import Analytics
import AppConfig
import ArgumentParser
import CLIOutput
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import WendyAgentGRPC

#if os(macOS)
    import AppKit
#endif

private struct SendableProgressUpdater: @unchecked Sendable {
    let call: (ProgressBarUpdate) -> Void

    init(_ call: @escaping (ProgressBarUpdate) -> Void) {
        self.call = call
    }

    @MainActor
    func update(_ value: Double, detail: String? = nil) {
        self.call(ProgressBarUpdate(progress: value, detail: detail))
    }
}

private actor UnpackProgressTracker {
    private var totalBytes: Int64 = 0
    private var totalLayers: Int = 0
    private var completedBytes: Int64 = 0
    private var completedLayers: Int = 0

    /// Returns a tuple of (progress value, detail string) for the given update.
    /// The detail string describes the current phase (e.g., "Layer 3/7").
    func progressValue(
        for update: Wendy_Agent_Services_V1_CreateContainerProgress
    ) -> (value: Double, detail: String?)? {
        switch update.phase {
        case .unpacking:
            // Start phase: capture total layers and total bytes
            if update.totalLayers > 0 {
                totalLayers = Int(update.totalLayers)
            }
            if update.layerSize > 0 {
                // In the start phase, layerSize contains the total image size
                totalBytes = update.layerSize
            }
            return (0, "Preparing")

        case .applyingLayer:
            // Layer completion: track progress
            if totalLayers == 0 && update.totalLayers > 0 {
                totalLayers = Int(update.totalLayers)
            }

            let layerIndex = Int(update.layerIndex)
            let layerSize = update.layerSize

            // Only count each layer once (layerIndex is 1-based)
            if layerIndex > completedLayers {
                completedLayers = layerIndex
                completedBytes += layerSize
            }

            let detail =
                totalLayers > 0
                ? "Layer \(layerIndex)/\(totalLayers)"
                : "Applying layers"

            // Prefer byte-based progress for accuracy (layers vary in size)
            if totalBytes > 0 {
                let value = Double(completedBytes) / Double(totalBytes)
                // Cap at 95% to leave room for finalization.
                return (min(max(value, 0), 0.95), detail)
            }

            // Fallback to layer-based progress
            if totalLayers > 0 {
                // Cap at 95% to leave room for finalization.
                let value = Double(layerIndex) / Double(totalLayers) * 0.95
                return (min(max(value, 0), 0.95), detail)
            }

            return (0, detail)

        case .creatingContainer:
            return (0.98, "Finalizing")

        case .complete:
            return (1, nil)

        case .unspecified, .UNRECOGNIZED:
            return nil
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
        case invalidProduct(String)
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
            case .invalidProduct(let name):
                return "No product named '\(name)' found in package"
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
                        cliOutput.info("Docker Desktop is installed")

                        guard
                            try await cliOutput.yesOrNoPrompt(
                                question: "Do you want to open Docker Desktop?",
                                defaultAnswer: true
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(url) {
                            cliOutput.info("Opening Docker.app")
                            while true {
                                do {
                                    _ = try await docker.getServerVersion()
                                    return
                                } catch {
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                            }
                        } else {
                            cliOutput.info(
                                "Failed to open Docker Desktop automatically, please open it manually"
                            )
                        }
                        return
                    } else if let url = NSWorkspace.shared.urlForApplication(
                        withBundleIdentifier: "com.orbstack.orbstack"
                    ) {
                        cliOutput.info("OrbStack.app is installed")

                        guard
                            try await cliOutput.yesOrNoPrompt(
                                question: "Do you want to open OrbStack?",
                                defaultAnswer: true
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(url) {
                            cliOutput.info("Opening OrbStack")
                            while true {
                                do {
                                    _ = try await docker.getServerVersion()
                                    return
                                } catch {
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                            }
                        } else {
                            cliOutput.info(
                                "Failed to open OrbStack automatically, please open it manually"
                            )
                        }
                        return
                    } else {
                        cliOutput.warning("Docker.app or OrbStack.app is not installed")
                        guard
                            try await cliOutput.yesOrNoPrompt(
                                question: "Do you want to open the installation guide?",
                                defaultAnswer: true
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(
                            URL(string: "https://docs.docker.com/get-docker/")!
                        ) {
                            cliOutput.info("Opening Docker documentation")
                        } else {
                            cliOutput.error("Failed to open Docker documentation")
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
                cliOutput.error(
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
                installSDK = try await cliOutput.yesOrNoPrompt(
                    question: "Do you want to install/update the WendyOS Swift SDK?",
                    defaultAnswer: true
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
                installSwift = try await cliOutput.yesOrNoPrompt(
                    question:
                        "Swift \(swiftVersion) is not installed yet. Do you want to install it?",
                    defaultAnswer: true
                )
            }

            if installSwift {
                try await cliOutput.withStreamingOutputBox(
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
        userArgs: [String] = [],
        progress: ((ProgressBarUpdate) -> Void)? = nil
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
            $0.userArgs = userArgs
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
                            if let result = await progressTracker.progressValue(
                                for: progressUpdate
                            ) {
                                await progressHandler.update(result.value, detail: result.detail)
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

            // Validate for unknown keys and emit warnings
            let warnings = AppConfig.validateJSON(appConfigData)
            for warning in warnings {
                cliOutput.warning(warning)
            }

            // Validate data can be decoded
            _ = try JSONDecoder().decode(AppConfig.self, from: appConfigData)
            return appConfigData
        } catch {
            logger.debug("Failed to decode app config", metadata: ["error": .string("\(error)")])
            cliOutput.info("No valid wendy.json was found. Using default settings.")
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
        commandName: String,
        additionalProperties: [String: String] = [:],
        operation: @Sendable () async throws -> T
    ) async throws -> T {
        do {
            return try await operation()
        } catch {
            await trackPhaseFailure(
                phase: phase,
                runtime: "containerd",
                commandName: commandName,
                error: error,
                additionalProperties: additionalProperties
            )
            throw error
        }
    }
}
