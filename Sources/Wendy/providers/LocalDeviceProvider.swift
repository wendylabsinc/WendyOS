import CLIOutput
import Foundation
import Subprocess
import WendyShared

#if os(macOS)
    import System
#else
    import SystemPackage
#endif

/// Build context for the local device provider
struct LocalBuildContext: Sendable {
    let executablePath: String
}

/// Device provider for building and running Swift packages on the local machine.
struct LocalDeviceProvider: DeviceProvider, Sendable {
    let key = "local"
    let displayName = "Local (This Device)"

    // MARK: - Availability

    func isAvailable() async -> Bool {
        true
    }

    // MARK: - Requirements

    func checkRequirements(shouldAutoAccept: Bool) async throws {
        // No-op: swift build will fail clearly if Swift is missing
    }

    // MARK: - Discovery

    func discoverDevices() async throws -> [ExternalDevice] {
        [
            ExternalDevice(
                id: "local",
                displayName: "Local (This Device)",
                providerKey: key,
                agentVersion: Version.current
            )
        ]
    }

    // MARK: - Build

    func canBuild(projectPath: URL) async -> Bool {
        FileManager.default.fileExists(
            atPath: projectPath.appendingPathComponent("Package.swift").path
        )
    }

    func build(
        for device: ExternalDevice,
        projectPath: URL,
        product: String,
        debug: Bool
    ) async throws -> ProviderBuiltApp {
        let swiftPM = SwiftPM()
        try await swiftPM.build(.product(product))

        let executablePath =
            projectPath
            .appendingPathComponent(".build/debug/\(product)")
            .path

        return ProviderBuiltApp(
            provider: self,
            device: device,
            appName: product,
            context: LocalBuildContext(executablePath: executablePath)
        )
    }

    // MARK: - Run

    func run(
        _ builtApp: ProviderBuiltApp,
        detach: Bool,
        output: AsyncStream<ProviderRunOutput>.Continuation
    ) async throws {
        guard let ctx = builtApp.context as? LocalBuildContext else {
            throw CLIError.invalidArgument(
                name: "context",
                value: "unknown",
                reason: "Invalid build context for local provider"
            )
        }

        output.yield(.started)

        _ = try await Subprocess.run(
            Subprocess.Executable.path(FilePath(ctx.executablePath)),
            arguments: [],
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        output.finish()
    }

    // MARK: - Stop

    func stop(_ builtApp: ProviderBuiltApp) async throws {
        // No-op: the process handles its own lifecycle
    }
}
