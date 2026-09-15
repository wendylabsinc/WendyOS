import Foundation

#if canImport(Metal)
    import Metal
#endif

/// Shared, injectable GPU evidence for device metadata and hardware inventory.
struct GPUDiscovery: Sendable {
    struct Device: Sendable {
        var name: String
        var vendor: String
        var computeBackends: [String]
    }

    var devices: @Sendable () -> [Device] = {
        #if canImport(Metal)
            MTLCopyAllDevices().map { device in
                let name = device.name.lowercased()
                let vendor =
                    name.contains("apple")
                    ? "apple"
                    : name.contains("amd")
                        ? "amd"
                        : name.contains("intel")
                            ? "intel"
                            : name.contains("nvidia") ? "nvidia" : "unknown"
                return Device(name: device.name, vendor: vendor, computeBackends: ["metal"])
            }
        #else
            []
        #endif
    }
}
