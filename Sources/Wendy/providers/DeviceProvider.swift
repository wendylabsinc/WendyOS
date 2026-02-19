import Foundation
import WendyShared

/// Protocol that all device provider plugins must conform to.
/// Each provider handles the full lifecycle for a specific target platform:
/// discovery, building, running, and stopping.
protocol DeviceProvider: Sendable {
    /// Unique key identifying this provider (e.g. "android", "esp32", "aws")
    var key: String { get }

    /// Human-readable name (e.g. "Android (ADB)")
    var displayName: String { get }

    /// Whether this provider's required tools are present on the host
    func isAvailable() async -> Bool

    /// Check and optionally install missing requirements (SDKs, tools)
    func checkRequirements(shouldAutoAccept: Bool) async throws

    /// Discover devices reachable by this provider
    func discoverDevices() async throws -> [ExternalDevice]

    /// Whether this provider can build the project at the given path
    func canBuild(projectPath: URL) async -> Bool

    /// Cross-compile or package the project for the given device
    func build(
        for device: ExternalDevice,
        projectPath: URL,
        product: String,
        debug: Bool
    ) async throws -> ProviderBuiltApp

    /// Deploy and run a previously built app, streaming output
    func run(
        _ builtApp: ProviderBuiltApp,
        detach: Bool,
        output: AsyncStream<ProviderRunOutput>.Continuation
    ) async throws

    /// Stop a running app
    func stop(_ builtApp: ProviderBuiltApp) async throws
}

/// The result of a provider build step. Carries enough context
/// for the same provider to run or stop the app later.
struct ProviderBuiltApp: Sendable {
    let provider: any DeviceProvider
    let device: ExternalDevice
    let appName: String
    /// Provider-specific build context (e.g. AndroidBuildContext with push path)
    let context: any Sendable
}

/// Output events streamed from a running provider app
enum ProviderRunOutput: Sendable {
    case stdout(Data)
    case stderr(Data)
    case started
}
