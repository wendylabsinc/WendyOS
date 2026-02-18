import Analytics
import AppConfig
import ArgumentParser
import CLIOutput
import ContainerRegistry
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIO
import Subprocess
import WendyAgentGRPC
import WendyShared

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

#if os(macOS)
    import AppKit
#endif

struct RunCommand: AsyncParsableCommand, Sendable {
    static let configuration = CommandConfiguration(
        commandName: "run",
        abstract: "Run Wendy projects."
    )

    @Flag(name: .long, help: "Attach a debugger to the container")
    var debug: Bool = false

    @Flag(name: .long, help: "Run the container in the background")
    var detach: Bool = false

    @Flag(name: .long, help: "Deploy mode with automatic restarts (up to 5 retries on failure)")
    var deploy: Bool = false

    @Flag(name: .customShort("y"), help: "Auto-accept prompts (required for --json mode)")
    var autoAccept: Bool = false

    /// Whether prompts should be auto-accepted (either explicit -y or JSON mode)
    var shouldAutoAccept: Bool { autoAccept || JSONMode.isEnabled }

    @Flag(name: .customLong("local"), help: "Run using a local profile on this machine")
    var local: Bool = false

    @Option(name: .customLong("profile"), help: "Run with a specific profile ID from wendy.json")
    var profile: String?

    @Flag(
        name: .customLong("print-command"),
        help: "Print resolved build/run commands, env, and working directory without executing"
    )
    var printCommand: Bool = false

    // Docker restart policy flags (mutually exclusive). Only applies to docker runtime.
    @Flag(name: .customLong("no-restart"), help: "Do not restart the container")
    var noRestart: Bool = false

    @Flag(name: .customLong("restart-unless-stopped"), help: "Restart unless stopped")
    var restartUnlessStoppedFlag: Bool = false

    @Option(
        name: .customLong("restart-on-failure"),
        help: "Restart on failure up to N times"
    )
    var restartOnFailureRetries: Int?

    @Argument(
        help: "The executable to run. Required when a package has multiple executable targets."
    )
    var executable: String?

    @OptionGroup
    var agentConnectionOptions: AgentConnectionOptions

    var swiftVersion: String { "6.2.3" }
    var swiftSDK: String { "\(swiftVersion)-RELEASE_wendyos_aarch64" }
    var sdkDownloadURL: String {
        "https://github.com/wendylabsinc/wendy-swift-tools/releases/download/0.4.0/\(swiftVersion)-RELEASE_wendyos_aarch64.artifactbundle.zip"
    }
    var sdkChecksum: String {
        "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e"
    }

    // Deploy mode should always run detached
    var isDetached: Bool { detach || deploy }

    /// Validate that flags are not conflicting
    func validate() throws {
        // Count how many restart policy flags are set
        var restartPolicyFlags: [String] = []

        if deploy {
            restartPolicyFlags.append("--deploy")
        }
        if noRestart {
            restartPolicyFlags.append("--no-restart")
        }
        if restartUnlessStoppedFlag {
            restartPolicyFlags.append("--restart-unless-stopped")
        }
        if restartOnFailureRetries != nil {
            restartPolicyFlags.append("--restart-on-failure")
        }

        // If more than one restart policy flag is set, show error
        if restartPolicyFlags.count > 1 {
            throw ValidationError(
                """
                Conflicting restart policy flags detected: \(restartPolicyFlags.joined(separator: ", "))

                Please use only one of:
                  --deploy                    (deploy mode with 5 retries on failure)
                  --no-restart                (never restart)
                  --restart-unless-stopped    (restart unless explicitly stopped)
                  --restart-on-failure N      (restart N times on failure)

                If no flag is provided, development mode is used (no restarts).
                """
            )
        }

        if local, agentConnectionOptions.device != nil {
            throw ValidationError("--local cannot be used together with --device")
        }
    }

    /// Build the restart policy based on the command flags
    /// This determines how containers behave when they exit
    func buildRestartPolicy() -> RestartPolicy {
        if noRestart {
            // Explicit no restart
            return .with { $0.mode = .no }
        } else if let retries = restartOnFailureRetries {
            // Custom retry count on failure
            return .with {
                $0.mode = .onFailure
                $0.onFailureMaxRetries = Int32(retries)
            }
        } else if restartUnlessStoppedFlag {
            // Restart unless explicitly stopped
            return .with { $0.mode = .unlessStopped }
        } else if deploy {
            // Deploy mode: retry up to 5 times on failure
            return .with {
                $0.mode = .onFailure
                $0.onFailureMaxRetries = 5
            }
        } else {
            // Default for development: no restarts
            return .with { $0.mode = .no }
        }
    }

    func run() async throws {
        try await withErrorTracking {
            // Validate flags before proceeding
            try validate()

            if let appConfig = loadValidAppConfig(), appConfig.hasProfiles {
                try await runWithProfiles(appConfig: appConfig)
                return
            }

            if printCommand {
                cliOutput.info(
                    "--print-command is currently available for profile-based runs in wendy.json."
                )
                return
            }

            try await BuildCommand(
                debug: debug,
                autoAccept: autoAccept,
                executable: executable,
                agentConnectionOptions: agentConnectionOptions
            ).withContainer(
                restartPolicy: buildRestartPolicy()
            ) { appName, client, endpoint in
                cliOutput.info("Starting container on \(endpoint.host)")
                try await AppBuildHelpers.executePhase(
                    phase: "start_container",
                    commandName: "wendy run"
                ) {
                    try await startContainerdContainer(
                        imageName: appName.name,
                        client: client,
                        hostname: endpoint.host
                    )
                }
            }
        }
    }

    private func loadValidAppConfig() -> AppConfig? {
        let configPath = URL(fileURLWithPath: "./wendy.json")
        guard FileManager.default.fileExists(atPath: configPath.path) else {
            return nil
        }

        do {
            let data = try Data(contentsOf: configPath)
            let warnings = AppConfig.validateJSON(data)
            for warning in warnings {
                cliOutput.warning(warning)
            }
            return try JSONDecoder().decode(AppConfig.self, from: data)
        } catch {
            cliOutput.warning(
                "Failed to parse wendy.json for profiles; falling back to legacy run behavior."
            )
            return nil
        }
    }

    private enum RunDestination: Sendable {
        case local
        case device(AgentConnectionOptions.Endpoint)
    }

    private struct RunDestinationOption: Sendable, Comparable {
        enum Kind: Sendable, Equatable {
            case local
            case device(host: String, port: Int)
        }

        let name: String
        let target: String
        let details: String
        let kind: Kind

        private var sortBucket: Int {
            switch kind {
            case .local: return 0
            case .device: return 1
            }
        }

        static func < (lhs: Self, rhs: Self) -> Bool {
            if lhs.sortBucket != rhs.sortBucket {
                return lhs.sortBucket < rhs.sortBucket
            }
            if lhs.name != rhs.name {
                return lhs.name < rhs.name
            }
            return lhs.details < rhs.details
        }
    }

    private struct HostExecutionPlan: Sendable {
        let executable: Executable
        let arguments: Arguments
        let workingDirectory: FilePath?
        let environment: [String: String]
        let displayCommand: String
    }

    private struct HookExecutionPlan: Sendable {
        let hook: AppConfig.Profile.Hook
        let phase: LifecycleHookPhase
        let index: Int
        let plan: HostExecutionPlan
    }

    private struct LifecycleHookPlans: Sendable {
        let preBuild: [HookExecutionPlan]
        let postBuild: [HookExecutionPlan]
        let preRun: [HookExecutionPlan]
        let postRun: [HookExecutionPlan]
        let preStop: [HookExecutionPlan]
    }

    private struct ProfileRuntimeContext: Sendable {
        let values: [String: String]
        let environment: [String: String]
    }

    private enum LifecycleHookPhase: String, Sendable {
        case preBuild
        case postBuild
        case preRun
        case postRun
        case preStop

        var displayName: String {
            switch self {
            case .preBuild: return "pre-build"
            case .postBuild: return "post-build"
            case .preRun: return "pre-run"
            case .postRun: return "post-run"
            case .preStop: return "pre-stop"
            }
        }
    }

    private func runWithProfiles(appConfig: AppConfig) async throws {
        let requestedProfile = try resolveRequestedProfile(appConfig: appConfig)
        let destination = try await resolveRunDestination(requestedProfile: requestedProfile)

        let context = profileResolutionContext(for: destination)
        let profile = try requestedProfile ?? appConfig.resolveProfile(context: context)
        let runtimeContext = makeRuntimeContext(profile: profile, destination: destination)

        switch destination {
        case .local:
            guard profile.when.target == .local else {
                throw CLIError.invalidConfig(
                    key: "profiles.\(profile.id).when.target",
                    reason:
                        "Resolved profile target is '\(profile.when.target.rawValue)', but local execution was requested"
                )
            }
            try await runLocalProfile(
                profile: profile,
                appConfig: appConfig,
                runtimeContext: runtimeContext
            )
        case .device(let endpoint):
            guard profile.when.target == .device else {
                throw CLIError.invalidConfig(
                    key: "profiles.\(profile.id).when.target",
                    reason:
                        "Resolved profile target is '\(profile.when.target.rawValue)', but device execution was requested"
                )
            }
            try await runDeviceProfile(
                profile: profile,
                appConfig: appConfig,
                endpoint: endpoint,
                runtimeContext: runtimeContext
            )
        }
    }

    private func resolveRequestedProfile(appConfig: AppConfig) throws -> AppConfig.Profile? {
        guard let profile else { return nil }
        guard let requested = appConfig.profile(withID: profile) else {
            throw ProfileResolutionError.profileNotFound(id: profile)
        }
        if requested.when.target == .remote {
            throw CLIError.unsupportedPlatform(
                reason: "Remote profiles are not supported yet in this release"
            )
        }
        return requested
    }

    private func resolveRunDestination(
        requestedProfile: AppConfig.Profile?
    ) async throws -> RunDestination {
        if local {
            return .local
        }

        if let endpoint = try await explicitDeviceEndpointIfSet() {
            return .device(endpoint)
        }

        if let requestedProfile {
            switch requestedProfile.when.target {
            case .local:
                return .local
            case .device:
                return .device(try await selectDeviceEndpointOnly())
            case .remote:
                throw CLIError.unsupportedPlatform(
                    reason: "Remote profiles are not supported yet in this release"
                )
            }
        }

        if JSONMode.isEnabled {
            throw CLIError.missingArgument(
                name: "local/device/profile",
                description: "Use --local, --device, or --profile when profiles are configured"
            )
        }

        return try await selectRunDestinationIncludingLocal()
    }

    private func explicitDeviceEndpointIfSet() async throws -> AgentConnectionOptions.Endpoint? {
        let hasExplicit =
            agentConnectionOptions.device != nil
            || ProcessInfo.processInfo.environment["WENDY_AGENT"] != nil

        guard hasExplicit else {
            return nil
        }

        let selectedDevice = try await agentConnectionOptions.read(
            title: "Which device do you want to run this app on?",
            readDefault: false,
            includeBluetooth: false
        )
        return try endpoint(from: selectedDevice)
    }

    private func selectDeviceEndpointOnly() async throws -> AgentConnectionOptions.Endpoint {
        if JSONMode.isEnabled {
            throw CLIError.missingArgument(
                name: "device",
                description: "Use --device <host> when selecting a device profile in JSON mode"
            )
        }

        let selectedDevice = try await agentConnectionOptions.read(
            title: "Which WendyOS device do you want to run this app on?",
            readDefault: false,
            includeBluetooth: false
        )
        return try endpoint(from: selectedDevice)
    }

    private func endpoint(
        from selectedDevice: SelectedDevice
    ) throws -> AgentConnectionOptions.Endpoint {
        switch selectedDevice {
        case .lan(let host, let port, let defaultDevice):
            return AgentConnectionOptions.Endpoint(
                host: host,
                port: port,
                defaultDevice: defaultDevice
            )
        case .bluetooth:
            throw CLIError.invalidEndpoint("Bluetooth endpoints are not supported for wendy run")
        }
    }

    private func selectRunDestinationIncludingLocal() async throws -> RunDestination {
        let localOption = RunDestinationOption(
            name: "Local (this machine)",
            target: "Local",
            details: "\(currentOSIdentifier()) / \(currentArchitectureIdentifier())",
            kind: .local
        )

        let (updates, continuation) = AsyncStream<DevicesCollection>.makeStream()

        return try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                await DiscoverCommand.runStreamingDiscovery(
                    deviceCache: DeviceCache(),
                    resolveBluetoothVersionInline: false,
                    skipVersionResolution: true,
                    continuation: continuation
                )
            }

            defer { group.cancelAll() }

            let selected = try await cliOutput.selectFromStreamingTable(
                initial: [localOption],
                updates: updates.map { collection in
                    self.runDestinationOptions(from: collection, localOption: localOption)
                },
                pageSize: 20,
                renderTable: { options in
                    (
                        headers: ["Name", "Target", "Details"],
                        rows: options.map { [$0.name, $0.target, $0.details] }
                    )
                }
            )

            switch selected.kind {
            case .local:
                return .local
            case .device(let host, let port):
                return .device(.init(host: host, port: port, defaultDevice: false))
            }
        }
    }

    private func runDestinationOptions(
        from collection: DevicesCollection,
        localOption: RunDestinationOption
    ) -> [RunDestinationOption] {
        var options: [RunDestinationOption] = [localOption]

        for device in collection.groupedDevices() {
            guard
                let lanDevice = device.interfaces.compactMap({
                    (interface: DevicesCollection.InterfaceInfo) -> LANDevice? in
                    guard case .lan(let lan) = interface else { return nil }
                    return lan
                }).first
            else {
                continue
            }

            let version = device.interfaces.compactMap(\.agentVersion).first
            let details =
                if let version, !version.isEmpty {
                    "\(lanDevice.hostname):\(lanDevice.port) (v\(version))"
                } else {
                    "\(lanDevice.hostname):\(lanDevice.port)"
                }

            options.append(
                RunDestinationOption(
                    name: device.name,
                    target: "Device",
                    details: details,
                    kind: .device(host: lanDevice.hostname, port: lanDevice.port)
                )
            )
        }

        return options
    }

    private func profileResolutionContext(
        for destination: RunDestination
    ) -> ProfileResolutionContext {
        switch destination {
        case .local:
            return ProfileResolutionContext(
                target: .local,
                os: currentOSIdentifier(),
                arch: currentArchitectureIdentifier(),
                traits: Set([currentOSIdentifier(), currentArchitectureIdentifier()])
            )
        case .device(let endpoint):
            return ProfileResolutionContext(
                target: .device,
                traits: inferDeviceTraits(fromHostname: endpoint.host),
                devicePlatform: inferDevicePlatform(fromHostname: endpoint.host),
                deviceHostname: endpoint.host
            )
        }
    }

    private func runLocalProfile(
        profile: AppConfig.Profile,
        appConfig: AppConfig,
        runtimeContext: ProfileRuntimeContext
    ) async throws {
        let hooks = try makeLifecycleHookPlans(
            profile: profile,
            appConfig: appConfig,
            runtimeContext: runtimeContext
        )
        let buildPlan = try makeBuildHostPlan(
            profile: profile,
            appConfig: appConfig,
            runtimeContext: runtimeContext
        )
        let runPlan = try makeRunHostPlan(
            profile: profile,
            appConfig: appConfig,
            runtimeContext: runtimeContext
        )

        if printCommand {
            printHookExecutionPlans(hooks.preBuild)
            if let buildPlan {
                printHostExecutionPlan(
                    title: "Resolved local build command",
                    plan: buildPlan
                )
            }
            printHookExecutionPlans(hooks.postBuild)
            printHookExecutionPlans(hooks.preRun)
            printHostExecutionPlan(
                title: "Resolved local run command",
                plan: runPlan
            )
            printHookExecutionPlans(hooks.postRun)
            printHookExecutionPlans(hooks.preStop)
            return
        }

        try await executeHookPlans(hooks.preBuild)

        if let buildPlan {
            cliOutput.info("Running local build for profile '\(profile.id)'")
            try await executeHostPlan(buildPlan, title: "Local build")
        }

        try await executeHookPlans(hooks.postBuild)
        try await executeHookPlans(hooks.preRun)

        cliOutput.info("Running profile '\(profile.id)' locally")
        do {
            try await executeHostPlan(runPlan, title: "Local run")
            try await executeHookPlans(hooks.postRun)
        } catch is CancellationError {
            await executeHookPlansBestEffort(hooks.preStop)
            throw CancellationError()
        }
    }

    private func runDeviceProfile(
        profile: AppConfig.Profile,
        appConfig: AppConfig,
        endpoint: AgentConnectionOptions.Endpoint,
        runtimeContext: ProfileRuntimeContext
    ) async throws {
        if let run = profile.run, run.type == .host {
            throw CLIError.invalidConfig(
                key: "profiles.\(profile.id).run.type",
                reason: "Device profiles cannot use run.type='host'"
            )
        }

        let hooks = try makeLifecycleHookPlans(
            profile: profile,
            appConfig: appConfig,
            runtimeContext: runtimeContext
        )

        let appName = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
            .lastPathComponent
            .lowercased()

        let build = profile.build
        let effectiveAppConfigData = try effectiveAppConfigData(
            appConfig: appConfig,
            profile: profile
        )

        let containerCommand = interpolateArray(
            profile.run?.container?.cmd,
            context: runtimeContext.values
        )
        let containerWorkingDir = interpolateOptionalString(
            profile.run?.container?.workingDir,
            context: runtimeContext.values
        )

        if printCommand {
            printHookExecutionPlans(hooks.preBuild)
            printResolvedDevicePlan(
                appName: appName,
                profile: profile,
                endpoint: endpoint,
                containerCommand: containerCommand,
                containerWorkingDir: containerWorkingDir,
                runtimeContext: runtimeContext
            )
            printHookExecutionPlans(hooks.postBuild)
            printHookExecutionPlans(hooks.preRun)
            printHookExecutionPlans(hooks.postRun)
            printHookExecutionPlans(hooks.preStop)
            return
        }

        try await executeHookPlans(hooks.preBuild)

        try await withAgentGRPCClient(endpoint, title: "Connecting to \(endpoint.host)") { client in
            if let build {
                switch build.type {
                case .none:
                    break
                case .docker:
                    try await AppBuildHelpers.checkDockerIsRunning(
                        shouldAutoAccept: shouldAutoAccept
                    )
                    let docker = DockerCLI()

                    try await AppBuildHelpers.executePhase(
                        phase: "builder_setup",
                        commandName: "wendy run"
                    ) {
                        try await cliOutput.withProgress(
                            message: "Preparing builder",
                            successMessage: "Builder ready",
                            errorMessage: "Failed to create builder"
                        ) {
                            try await docker.prepareBuildxBuilder(
                                registryHostname: endpoint.host,
                                registryPort: 5000
                            )
                        }
                    }

                    try await AppBuildHelpers.executePhase(
                        phase: "build_upload",
                        commandName: "wendy run"
                    ) {
                        try await docker.buildxAndPush(
                            name: appName,
                            registryHostname: endpoint.host,
                            registryPort: 5000,
                            platform: interpolateString(
                                build.platform ?? "linux/arm64",
                                context: runtimeContext.values
                            ),
                            dockerfile: interpolateOptionalString(
                                build.dockerfile,
                                context: runtimeContext.values
                            ),
                            buildArgs: interpolateDictionary(
                                build.buildArgs,
                                context: runtimeContext.values
                            ) ?? [:],
                            environment: mergedEnvironment(
                                appConfig: appConfig,
                                profile: profile,
                                extra: build.env,
                                contextEnvironment: runtimeContext.environment
                            )
                        )
                    }
                case .command:
                    let buildPlan = try hostPlanFromBuildCommand(
                        build: build,
                        appConfig: appConfig,
                        profile: profile,
                        runtimeContext: runtimeContext
                    )
                    cliOutput.info("Running device build command for profile '\(profile.id)'")
                    try await executeHostPlan(buildPlan, title: "Device build")
                }
            }

            try await executeHookPlans(hooks.postBuild)
            try await executeHookPlans(hooks.preRun)

            try await AppBuildHelpers.executePhase(
                phase: "prepare_container",
                commandName: "wendy run"
            ) {
                try await cliOutput.withLabeledProgressBar(
                    message: "Unpacking image on device"
                ) { updateProgress in
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: appName,
                        client: client,
                        restartPolicy: buildRestartPolicy(),
                        progress: updateProgress,
                        appConfigDataOverride: effectiveAppConfigData,
                        containerCommand: containerCommand,
                        containerWorkingDir: containerWorkingDir
                    )
                }
            }

            cliOutput.info("Starting container on \(endpoint.host)")
            try await AppBuildHelpers.executePhase(
                phase: "start_container",
                commandName: "wendy run"
            ) {
                try await startContainerdContainer(
                    imageName: appName,
                    client: client,
                    hostname: endpoint.host,
                    onBeforeStop: {
                        await executeHookPlansBestEffort(hooks.preStop)
                    }
                )
            }

            try await executeHookPlans(hooks.postRun)
        }
    }

    private func makeBuildHostPlan(
        profile: AppConfig.Profile,
        appConfig: AppConfig,
        runtimeContext: ProfileRuntimeContext
    ) throws -> HostExecutionPlan? {
        guard let build = profile.build else {
            return nil
        }

        switch build.type {
        case .none:
            return nil
        case .docker:
            throw CLIError.invalidConfig(
                key: "profiles.\(profile.id).build.type",
                reason: "Local profiles do not support build.type='docker'"
            )
        case .command:
            return try hostPlanFromBuildCommand(
                build: build,
                appConfig: appConfig,
                profile: profile,
                runtimeContext: runtimeContext
            )
        }
    }

    private func makeRunHostPlan(
        profile: AppConfig.Profile,
        appConfig: AppConfig,
        runtimeContext: ProfileRuntimeContext
    ) throws -> HostExecutionPlan {
        guard let run = profile.run else {
            throw CLIError.invalidConfig(
                key: "profiles.\(profile.id).run",
                reason: "Local profiles must define a run section"
            )
        }
        guard run.type == .host else {
            throw CLIError.invalidConfig(
                key: "profiles.\(profile.id).run.type",
                reason: "Local profiles must use run.type='host'"
            )
        }
        return try hostPlanFromRunCommand(
            run: run,
            appConfig: appConfig,
            profile: profile,
            runtimeContext: runtimeContext
        )
    }

    private func hostPlanFromBuildCommand(
        build: AppConfig.Profile.Build,
        appConfig: AppConfig,
        profile: AppConfig.Profile,
        runtimeContext: ProfileRuntimeContext
    ) throws -> HostExecutionPlan {
        guard let command = build.command, !command.isEmpty else {
            throw CLIError.invalidConfig(
                key: "profiles.\(profile.id).build.command",
                reason: "build.type='command' requires a non-empty command"
            )
        }

        let mergedEnvironment = mergedEnvironment(
            appConfig: appConfig,
            profile: profile,
            extra: build.env,
            contextEnvironment: runtimeContext.environment
        )
        let workingDirectory = resolveWorkingDirectory(
            runCWD: interpolateOptionalString(build.cwd, context: runtimeContext.values),
            fallbackCWD: appConfig.workingDir
        )

        return try hostPlanFromCommandOrArgs(
            command: interpolateString(command, context: runtimeContext.values),
            shell: interpolateOptionalString(build.shell, context: runtimeContext.values),
            args: interpolateArray(build.args, context: runtimeContext.values),
            workingDirectory: workingDirectory,
            environment: mergedEnvironment,
            key: "profiles.\(profile.id).build.command",
            reason: "build.type='command' requires a non-empty command"
        )
    }

    private func hostPlanFromRunCommand(
        run: AppConfig.Profile.Run,
        appConfig: AppConfig,
        profile: AppConfig.Profile,
        runtimeContext: ProfileRuntimeContext
    ) throws -> HostExecutionPlan {
        let mergedEnvironment = mergedEnvironment(
            appConfig: appConfig,
            profile: profile,
            extra: interpolateDictionary(run.env, context: runtimeContext.values),
            contextEnvironment: runtimeContext.environment
        )
        let workingDirectory = resolveWorkingDirectory(
            runCWD: interpolateOptionalString(run.cwd, context: runtimeContext.values),
            fallbackCWD: appConfig.workingDir
        )

        return try hostPlanFromCommandOrArgs(
            command: interpolateOptionalString(run.command, context: runtimeContext.values),
            shell: interpolateOptionalString(run.shell, context: runtimeContext.values),
            args: interpolateArray(run.args, context: runtimeContext.values),
            workingDirectory: workingDirectory,
            environment: mergedEnvironment,
            key: "profiles.\(profile.id).run",
            reason: "run.type='host' requires either 'command' or non-empty 'args'"
        )
    }

    private func hostPlanFromCommandOrArgs(
        command: String?,
        shell: String?,
        args: [String]?,
        workingDirectory: FilePath?,
        environment: [String: String],
        key: String,
        reason: String
    ) throws -> HostExecutionPlan {
        if let command, !command.isEmpty {
            let shell = shell ?? defaultShellExecutableName()
            let commandWithArgs = appendArguments(command: command, args: args, shell: shell)
            let shellArgs = Self.shellInvocationArguments(shell: shell, command: commandWithArgs)
            return HostExecutionPlan(
                executable: .name(shell),
                arguments: Arguments(shellArgs),
                workingDirectory: workingDirectory,
                environment: environment,
                displayCommand: ([shell] + shellArgs).joined(separator: " ")
            )
        }

        if let args, let executable = args.first {
            return HostExecutionPlan(
                executable: .name(executable),
                arguments: Arguments(Array(args.dropFirst())),
                workingDirectory: workingDirectory,
                environment: environment,
                displayCommand: ([executable] + Array(args.dropFirst())).joined(separator: " ")
            )
        }

        throw CLIError.invalidConfig(
            key: key,
            reason: reason
        )
    }

    private func executeHostPlan(_ plan: HostExecutionPlan, title: String) async throws {
        _ = try await cliOutput.withStreamingOutput(title: title) { emit in
            try await wendy.run(
                executable: plan.executable,
                arguments: plan.arguments,
                workingDirectory: plan.workingDirectory,
                environment: plan.environment
            ) { chunk in
                try await emit(chunk)
            }
        }
    }

    private func printHostExecutionPlan(title: String, plan: HostExecutionPlan) {
        cliOutput.info(title)
        cliOutput.info("  command: \(plan.displayCommand)")
        if let workingDirectory = plan.workingDirectory {
            cliOutput.info("  cwd: \(String(describing: workingDirectory))")
        }
        if !plan.environment.isEmpty {
            let envSummary = plan.environment
                .keys
                .sorted()
                .map { key in
                    "\(key)=\(plan.environment[key] ?? "")"
                }
                .joined(separator: ", ")
            cliOutput.info("  env: \(envSummary)")
        }
    }

    private func printResolvedDevicePlan(
        appName: String,
        profile: AppConfig.Profile,
        endpoint: AgentConnectionOptions.Endpoint,
        containerCommand: [String]?,
        containerWorkingDir: String?,
        runtimeContext: ProfileRuntimeContext
    ) {
        cliOutput.info("Resolved device run plan")
        cliOutput.info("  profile: \(profile.id)")
        cliOutput.info("  device: \(endpoint.host):\(endpoint.port)")
        cliOutput.info("  image: \(appName):latest")

        if let build = profile.build {
            switch build.type {
            case .none:
                cliOutput.info("  build: none")
            case .docker:
                cliOutput.info(
                    "  build: docker (dockerfile=\(interpolateOptionalString(build.dockerfile, context: runtimeContext.values) ?? "Dockerfile"), platform=\(interpolateString(build.platform ?? "linux/arm64", context: runtimeContext.values)))"
                )
                let buildArgs = interpolateDictionary(
                    build.buildArgs,
                    context: runtimeContext.values
                )
                if let buildArgs, !buildArgs.isEmpty {
                    let formatted = buildArgs
                        .keys
                        .sorted()
                        .map { key in "\(key)=\(buildArgs[key] ?? "")" }
                        .joined(separator: ", ")
                    cliOutput.info("  buildArgs: \(formatted)")
                }
                if let buildEnv = interpolateDictionary(build.env, context: runtimeContext.values),
                    !buildEnv.isEmpty
                {
                    let formatted = buildEnv
                        .keys
                        .sorted()
                        .map { key in "\(key)=\(buildEnv[key] ?? "")" }
                        .joined(separator: ", ")
                    cliOutput.info("  build env: \(formatted)")
                }
            case .command:
                cliOutput.info("  build: command")
                if let command = interpolateOptionalString(
                    build.command,
                    context: runtimeContext.values
                ) {
                    cliOutput.info("  build command: \(command)")
                }
            }
        }

        if let containerCommand, !containerCommand.isEmpty {
            cliOutput.info("  container cmd: \(containerCommand.joined(separator: " "))")
        }
        if let containerWorkingDir, !containerWorkingDir.isEmpty {
            cliOutput.info("  container workingDir: \(containerWorkingDir)")
        }
    }

    private func makeRuntimeContext(
        profile: AppConfig.Profile,
        destination: RunDestination
    ) -> ProfileRuntimeContext {
        let hostOS = currentOSIdentifier()
        let hostArch = currentArchitectureIdentifier()

        let targetKind: String
        let targetOS: String
        let targetArch: String
        let deviceHostTemplate: String
        let deviceHostEnvironment: String
        let devicePort: String
        let devicePlatform: String

        switch destination {
        case .local:
            targetKind = "local"
            targetOS = hostOS
            targetArch = hostArch
            deviceHostTemplate = ""
            deviceHostEnvironment = ""
            devicePort = ""
            devicePlatform = ""
        case .device(let endpoint):
            targetKind = "device"
            targetOS = "linux"
            targetArch = inferDeviceArchitecture(fromHostname: endpoint.host)
            deviceHostTemplate = Self.sanitizeTemplateDeviceHost(endpoint.host)
            deviceHostEnvironment = endpoint.host
            devicePort = String(endpoint.port)
            devicePlatform = inferDevicePlatform(fromHostname: endpoint.host) ?? "generic-linux"
        }

        let targetPlatform = "\(targetOS)/\(targetArch)"
        let values: [String: String] = [
            "profile.id": profile.id,
            "host.os": hostOS,
            "host.arch": hostArch,
            "target.kind": targetKind,
            "target.os": targetOS,
            "target.arch": targetArch,
            "target.platform": targetPlatform,
            "device.host": deviceHostTemplate,
            "device.port": devicePort,
            "device.platform": devicePlatform,
        ]

        let environment: [String: String] = [
            "WENDY_PROFILE_ID": profile.id,
            "WENDY_HOST_OS": hostOS,
            "WENDY_HOST_ARCH": hostArch,
            "WENDY_TARGET_KIND": targetKind,
            "WENDY_TARGET_OS": targetOS,
            "WENDY_TARGET_ARCH": targetArch,
            "WENDY_TARGET_PLATFORM": targetPlatform,
            "WENDY_DEVICE_HOST": deviceHostEnvironment,
            "WENDY_DEVICE_PORT": devicePort,
            "WENDY_DEVICE_PLATFORM": devicePlatform,
        ]

        return ProfileRuntimeContext(values: values, environment: environment)
    }

    static func sanitizeTemplateDeviceHost(_ rawHost: String) -> String {
        let trimmed = rawHost.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return "" }

        let allowed = CharacterSet(
            charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-"
        )
        var scalarView = String.UnicodeScalarView()
        scalarView.reserveCapacity(trimmed.unicodeScalars.count)
        var previousWasDash = false

        for scalar in trimmed.unicodeScalars {
            if allowed.contains(scalar) {
                if scalar == "-", previousWasDash {
                    continue
                }
                scalarView.append(scalar)
                previousWasDash = scalar == "-"
                continue
            }

            if !previousWasDash {
                scalarView.append("-")
                previousWasDash = true
            }
        }

        let sanitized = String(scalarView).trimmingCharacters(in: CharacterSet(charactersIn: ".-"))
        return sanitized.isEmpty ? "unknown-device" : sanitized
    }

    private func interpolateString(_ value: String, context: [String: String]) -> String {
        let pattern = #"\{\{\s*([a-zA-Z0-9_.-]+)\s*\}\}"#
        guard let regex = try? NSRegularExpression(pattern: pattern) else {
            return value
        }

        let range = NSRange(value.startIndex..<value.endIndex, in: value)
        let matches = regex.matches(in: value, range: range).reversed()
        guard !matches.isEmpty else { return value }

        var resolved = value
        for match in matches {
            guard match.numberOfRanges > 1 else { continue }
            guard
                let fullRange = Range(match.range(at: 0), in: resolved),
                let keyRange = Range(match.range(at: 1), in: resolved)
            else {
                continue
            }
            let key = String(resolved[keyRange])
            if let replacement = context[key] {
                resolved.replaceSubrange(fullRange, with: replacement)
            }
        }

        return resolved
    }

    private func interpolateOptionalString(
        _ value: String?,
        context: [String: String]
    ) -> String? {
        guard let value else { return nil }
        return interpolateString(value, context: context)
    }

    private func interpolateArray(_ values: [String]?, context: [String: String]) -> [String]? {
        guard let values else { return nil }
        return values.map { interpolateString($0, context: context) }
    }

    private func interpolateDictionary(
        _ values: [String: String]?,
        context: [String: String]
    ) -> [String: String]? {
        guard let values else { return nil }
        var resolved: [String: String] = [:]
        for key in values.keys.sorted() {
            guard let value = values[key] else { continue }
            resolved[key] = interpolateString(value, context: context)
        }
        return resolved
    }

    private func hooks(
        for phase: LifecycleHookPhase,
        from profile: AppConfig.Profile
    ) -> [AppConfig.Profile.Hook] {
        guard let hooks = profile.hooks else { return [] }

        switch phase {
        case .preBuild:
            return hooks.preBuild ?? []
        case .postBuild:
            return hooks.postBuild ?? []
        case .preRun:
            return hooks.preRun ?? []
        case .postRun:
            return hooks.postRun ?? []
        case .preStop:
            return hooks.preStop ?? []
        }
    }

    private func makeLifecycleHookPlans(
        profile: AppConfig.Profile,
        appConfig: AppConfig,
        runtimeContext: ProfileRuntimeContext
    ) throws -> LifecycleHookPlans {
        LifecycleHookPlans(
            preBuild: try makeHookPlans(
                phase: .preBuild,
                profile: profile,
                appConfig: appConfig,
                runtimeContext: runtimeContext,
                stageEnvironment: profile.build?.env
            ),
            postBuild: try makeHookPlans(
                phase: .postBuild,
                profile: profile,
                appConfig: appConfig,
                runtimeContext: runtimeContext,
                stageEnvironment: profile.build?.env
            ),
            preRun: try makeHookPlans(
                phase: .preRun,
                profile: profile,
                appConfig: appConfig,
                runtimeContext: runtimeContext,
                stageEnvironment: profile.run?.env
            ),
            postRun: try makeHookPlans(
                phase: .postRun,
                profile: profile,
                appConfig: appConfig,
                runtimeContext: runtimeContext,
                stageEnvironment: profile.run?.env
            ),
            preStop: try makeHookPlans(
                phase: .preStop,
                profile: profile,
                appConfig: appConfig,
                runtimeContext: runtimeContext,
                stageEnvironment: profile.run?.env
            )
        )
    }

    private func makeHookPlans(
        phase: LifecycleHookPhase,
        profile: AppConfig.Profile,
        appConfig: AppConfig,
        runtimeContext: ProfileRuntimeContext,
        stageEnvironment: [String: String]?
    ) throws -> [HookExecutionPlan] {
        try hooks(for: phase, from: profile).enumerated().map { index, hook in
            HookExecutionPlan(
                hook: hook,
                phase: phase,
                index: index,
                plan: try hostPlanFromHook(
                    hook: hook,
                    phase: phase,
                    index: index,
                    appConfig: appConfig,
                    profile: profile,
                    runtimeContext: runtimeContext,
                    stageEnvironment: stageEnvironment
                )
            )
        }
    }

    private func hostPlanFromHook(
        hook: AppConfig.Profile.Hook,
        phase: LifecycleHookPhase,
        index: Int,
        appConfig: AppConfig,
        profile: AppConfig.Profile,
        runtimeContext: ProfileRuntimeContext,
        stageEnvironment: [String: String]?
    ) throws -> HostExecutionPlan {
        let keyPrefix = "profiles.\(profile.id).hooks.\(phase.rawValue)[\(index)]"
        var extraEnvironment =
            interpolateDictionary(stageEnvironment, context: runtimeContext.values)
            ?? [:]
        if let hookEnvironment = interpolateDictionary(hook.env, context: runtimeContext.values) {
            for (key, value) in hookEnvironment {
                extraEnvironment[key] = value
            }
        }
        let mergedEnvironment = mergedEnvironment(
            appConfig: appConfig,
            profile: profile,
            extra: extraEnvironment.isEmpty ? nil : extraEnvironment,
            contextEnvironment: runtimeContext.environment
        )
        let workingDirectory = resolveWorkingDirectory(
            runCWD: interpolateOptionalString(hook.cwd, context: runtimeContext.values),
            fallbackCWD: appConfig.workingDir
        )

        return try hostPlanFromCommandOrArgs(
            command: interpolateOptionalString(hook.command, context: runtimeContext.values),
            shell: interpolateOptionalString(hook.shell, context: runtimeContext.values),
            args: interpolateArray(hook.args, context: runtimeContext.values),
            workingDirectory: workingDirectory,
            environment: mergedEnvironment,
            key: keyPrefix,
            reason:
                "Each hook must define a non-empty 'command' or executable-style non-empty 'args'"
        )
    }

    private func executeHookPlans(_ hookPlans: [HookExecutionPlan]) async throws {
        for hookPlan in hookPlans {
            let name = hookPlan.hook.name ?? "#\(hookPlan.index + 1)"
            do {
                try await executeHostPlan(
                    hookPlan.plan,
                    title: "Hook \(hookPlan.phase.displayName): \(name)"
                )
            } catch {
                if hookPlan.hook.continueOnError ?? false {
                    cliOutput.warning(
                        "Hook \(hookPlan.phase.displayName) '\(name)' failed but continueOnError=true"
                    )
                    continue
                }
                throw error
            }
        }
    }

    private func executeHookPlansBestEffort(_ hookPlans: [HookExecutionPlan]) async {
        for hookPlan in hookPlans {
            let name = hookPlan.hook.name ?? "#\(hookPlan.index + 1)"
            do {
                try await executeHostPlan(
                    hookPlan.plan,
                    title: "Hook \(hookPlan.phase.displayName): \(name)"
                )
            } catch {
                cliOutput.warning(
                    "Best-effort hook \(hookPlan.phase.displayName) '\(name)' failed: \(error.localizedDescription)"
                )
            }
        }
    }

    private func printHookExecutionPlans(_ hookPlans: [HookExecutionPlan]) {
        for hookPlan in hookPlans {
            let name = hookPlan.hook.name ?? "#\(hookPlan.index + 1)"
            printHostExecutionPlan(
                title: "Resolved \(hookPlan.phase.displayName) hook: \(name)",
                plan: hookPlan.plan
            )
        }
    }

    private func mergedEnvironment(
        appConfig: AppConfig,
        profile: AppConfig.Profile,
        extra: [String: String]?,
        contextEnvironment: [String: String]
    ) -> [String: String] {
        var merged: [String: String] = appConfig.env ?? [:]

        if let profileEnv = profile.env {
            for (key, value) in profileEnv {
                merged[key] = value
            }
        }

        if let extra {
            for (key, value) in extra {
                merged[key] = value
            }
        }

        for (key, value) in contextEnvironment {
            merged[key] = value
        }

        let otelEnabled = profile.otel?.enabled ?? true
        if otelEnabled {
            if merged["OTEL_SERVICE_NAME"] == nil {
                merged["OTEL_SERVICE_NAME"] = profile.otel?.serviceName ?? appConfig.appId
            }
            if merged["OTEL_EXPORTER_OTLP_ENDPOINT"] == nil {
                merged["OTEL_EXPORTER_OTLP_ENDPOINT"] =
                    profile.otel?.endpoint ?? "http://127.0.0.1:4318"
            }
        }

        return merged
    }

    private func effectiveAppConfigData(
        appConfig: AppConfig,
        profile: AppConfig.Profile
    ) throws -> Data {
        var effective = appConfig
        if let profileEntitlements = profile.entitlements {
            effective.entitlements = profileEntitlements
        }
        return try JSONEncoder().encode(effective)
    }

    private func resolveWorkingDirectory(runCWD: String?, fallbackCWD: String?) -> FilePath? {
        let cwd = runCWD ?? fallbackCWD
        guard let cwd, !cwd.isEmpty else {
            return nil
        }
        return FilePath(cwd)
    }

    private func appendArguments(command: String, args: [String]?, shell: String) -> String {
        guard let args, !args.isEmpty else {
            return command
        }
        return command + " "
            + args.map { Self.shellEscape($0, shell: shell) }.joined(separator: " ")
    }

    static func shellEscape(_ argument: String, shell: String? = nil) -> String {
        #if os(Windows)
            let lower = (shell ?? "").lowercased()
            if lower.contains("powershell") || lower == "pwsh" || lower == "pwsh.exe" {
                return "'\(argument.replacingOccurrences(of: "'", with: "''"))'"
            }
            let cmdSpecials = CharacterSet(charactersIn: "^&|<>()%!\"")
            let requiresQuotes =
                argument.isEmpty
                || argument.rangeOfCharacter(from: .whitespacesAndNewlines) != nil
                || argument.rangeOfCharacter(from: cmdSpecials) != nil
            var escaped = argument
            for special in ["^", "&", "|", "<", ">", "(", ")", "%", "!", "\""] {
                escaped = escaped.replacingOccurrences(of: special, with: "^\(special)")
            }
            return requiresQuotes ? "\"\(escaped)\"" : escaped
        #else
            let escaped = argument.replacingOccurrences(of: "'", with: "'\\''")
            return "'\(escaped)'"
        #endif
    }

    static func shellInvocationArguments(shell: String, command: String) -> [String] {
        let lower = shell.lowercased()
        if lower.contains("powershell") || lower == "pwsh" {
            return ["-Command", command]
        }
        if lower == "cmd" || lower == "cmd.exe" {
            return ["/C", command]
        }
        if lower.contains("fish") {
            return ["-c", command]
        }
        return ["-lc", command]
    }

    private func defaultShellExecutableName() -> String {
        #if os(Windows)
            return "powershell.exe"
        #else
            return ProcessInfo.processInfo.environment["SHELL"] ?? "sh"
        #endif
    }

    private func currentOSIdentifier() -> String {
        #if os(macOS)
            return "macos"
        #elseif os(Linux)
            return "linux"
        #elseif os(Windows)
            return "windows"
        #else
            return "unknown"
        #endif
    }

    private func currentArchitectureIdentifier() -> String {
        #if arch(arm64)
            return "arm64"
        #elseif arch(x86_64)
            return "x86_64"
        #elseif arch(i386)
            return "i386"
        #else
            return "unknown"
        #endif
    }

    private func inferDevicePlatform(fromHostname hostname: String) -> String? {
        let lower = hostname.lowercased()
        if lower.contains("jetson") {
            return "jetson"
        }
        if lower.contains("rpi") || lower.contains("raspberry") {
            return "generic-linux"
        }
        if lower.contains("linux") {
            return "generic-linux"
        }
        return nil
    }

    private func inferDeviceArchitecture(fromHostname hostname: String) -> String {
        let lower = hostname.lowercased()
        if lower.contains("x86_64") || lower.contains("amd64") {
            return "x86_64"
        }
        if lower.contains("armv7") || lower.contains("armhf") {
            return "armv7"
        }
        return "arm64"
    }

    private func inferDeviceTraits(fromHostname hostname: String) -> Set<String> {
        let lower = hostname.lowercased()
        if lower.contains("jetson") {
            return ["cuda"]
        }
        return ["cpu"]
    }

    /// Gracefully stop a container with timeout
    private func stopContainerWithTimeout(
        imageName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        timeout: TimeInterval = 5.0
    ) async {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.stop")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        do {
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask {
                    _ = try await agentContainers.stopContainer(
                        request: .init(
                            message: .with {
                                $0.appName = imageName
                            }
                        )
                    )
                }

                group.addTask {
                    try await Task.sleep(nanoseconds: UInt64(timeout * 1_000_000_000))
                    throw CancellationError()
                }

                // Wait for first task to complete (either stop succeeds or timeout)
                try await group.next()
                group.cancelAll()
            }
            logger.info("Container stopped successfully")
        } catch is CancellationError {
            logger.warning(
                "Stop container operation timed out after \(timeout)s",
                metadata: ["container": "\(imageName)"]
            )
        } catch {
            logger.error(
                "Failed to stop container",
                metadata: ["container": "\(imageName)", "error": "\(error)"]
            )
        }
    }

    func startContainerdContainer(
        imageName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        hostname: String,
        onBeforeStop: (@Sendable () async -> Void)? = nil
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.start")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        do {
            _ = try await agentContainers.startContainer(
                request: .init(
                    message: .with {
                        $0.appName = imageName
                    }
                )
            ) { response in
                for try await message in response.messages {
                    switch message.responseType {
                    case .started:
                        if debug {
                            cliOutput.success(
                                "Started app \(imageName) on \(hostname) with debug port 4242"
                            )
                        } else {
                            cliOutput.success(
                                "Started app \(imageName) on \(hostname)"
                            )
                        }

                        if isDetached {
                            return
                        }
                    case .stdoutOutput(let stdoutOutput):
                        stdoutOutput.data.withUnsafeBytes { data in
                            #if os(Windows)
                                _ = _write(STDOUT_FILENO, data.baseAddress!, UInt32(data.count))
                            #else
                                _ = write(STDOUT_FILENO, data.baseAddress!, data.count)
                            #endif
                        }
                    case .stderrOutput(let stderrOutput):
                        stderrOutput.data.withUnsafeBytes { data in
                            #if os(Windows)
                                _ = _write(STDERR_FILENO, data.baseAddress!, UInt32(data.count))
                            #else
                                _ = write(STDERR_FILENO, data.baseAddress!, data.count)
                            #endif
                        }
                    default:
                        logger.warning("Unknown message received from agent")
                    }
                }
            }
        } catch {
            // Handle any error (cancellation, network issues, etc.): stop the container when in development mode
            if !isDetached {
                let isCancellation = error is CancellationError
                logger.info(
                    "Container execution \(isCancellation ? "cancelled" : "failed"), stopping container",
                    metadata: ["container": "\(imageName)", "error": "\(error)"]
                )
                if let onBeforeStop {
                    await onBeforeStop()
                }
                await stopContainerWithTimeout(
                    imageName: imageName,
                    client: client,
                    timeout: 5.0
                )
            }
            throw error
        }
    }
}
