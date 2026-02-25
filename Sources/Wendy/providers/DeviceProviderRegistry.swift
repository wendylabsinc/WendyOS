import Foundation
import WendyShared

/// Compile-time registry of all device providers.
/// Call `initialize()` once at CLI startup to filter by availability.
enum DeviceProviderRegistry {
    /// All registered providers (before availability filtering)
    private static let allProviders: [any DeviceProvider] = [
        LocalDeviceProvider(),
        DockerDeviceProvider(),
        AndroidDeviceProvider(),
        MicroWendyDeviceProvider(),
    ]

    /// Providers that passed the `isAvailable()` check at startup.
    /// Safety: written once in `initialize()` before any concurrent reads.
    nonisolated(unsafe) private static var _availableProviders: [any DeviceProvider] = []

    /// Providers available on this host
    static var availableProviders: [any DeviceProvider] { _availableProviders }

    /// Filter providers by availability. Called once at CLI startup.
    static func initialize() async {
        var available = [any DeviceProvider]()
        for provider in allProviders {
            if await provider.isAvailable() {
                available.append(provider)
            }
        }
        _availableProviders = available
    }

    /// Look up an available provider by its key
    static func provider(forKey key: String) -> (any DeviceProvider)? {
        _availableProviders.first { $0.key == key }
    }
}
