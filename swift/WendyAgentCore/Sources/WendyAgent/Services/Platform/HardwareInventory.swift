import Foundation

/// A single discovered hardware capability, matching the proto
/// `ListHardwareCapabilitiesResponse.HardwareCapability` shape.
struct HardwareCapability: Sendable, Equatable {
    var category: String
    var devicePath: String
    var description: String
    var properties: [String: String]
}

/// Discovers hardware capabilities on the host.
protocol HardwareDiscovering: Sendable {
    /// - Parameter categoryFilter: when non-nil, only capabilities whose
    ///   `category` equals this value are returned.
    func discover(categoryFilter: String?) async throws -> [HardwareCapability]
}

/// Live macOS implementation backed by `system_profiler -json` and `getifaddrs`.
///
/// Linux-only categories (`i2c`, `spi`, `gpio`) have no macOS analogue and are
/// intentionally omitted.
struct HardwareInventory: HardwareDiscovering {
    private let systemProfilerPath = "/usr/sbin/system_profiler"
    var gpuDiscovery = GPUDiscovery()

    func discover(categoryFilter: String?) async throws -> [HardwareCapability] {
        async let displays = dataType("SPDisplaysDataType")
        async let usb = dataType("SPUSBDataType")
        async let camera = dataType("SPCameraDataType")
        async let audio = dataType("SPAudioDataType")
        async let storage = dataType("SPStorageDataType")

        var caps = Self.parseSystemProfiler(
            displays: await displays,
            usb: await usb,
            camera: await camera,
            audio: await audio,
            storage: await storage
        )
        let gpus = gpuDiscovery.devices()
        if !gpus.isEmpty {
            caps.removeAll { $0.category == "gpu" }
            caps.append(
                contentsOf: gpus.map { gpu in
                    HardwareCapability(
                        category: "gpu",
                        devicePath: "",
                        description: gpu.name,
                        properties: [
                            "vendor": gpu.vendor,
                            "compute_backends": gpu.computeBackends.joined(separator: ","),
                        ]
                    )
                }
            )
        }
        caps.append(contentsOf: Self.networkCapabilities())

        if let categoryFilter, !categoryFilter.isEmpty {
            caps = caps.filter { $0.category == categoryFilter }
        }
        return caps
    }

    private func dataType(_ name: String) async -> Data? {
        guard
            let result = try? await Subprocess.run(
                systemProfilerPath,
                ["-json", name],
                timeout: .seconds(20)
            ),
            result.status == 0
        else {
            return nil
        }
        return Data(result.stdout.utf8)
    }

    // MARK: - Pure parsing (fixture-testable)

    static func parseSystemProfiler(
        displays: Data?,
        usb: Data?,
        camera: Data?,
        audio: Data?,
        storage: Data?
    ) -> [HardwareCapability] {
        var caps: [HardwareCapability] = []
        caps.append(contentsOf: parseDisplays(displays))
        caps.append(contentsOf: parseUSB(usb))
        caps.append(contentsOf: parseCamera(camera))
        caps.append(contentsOf: parseAudio(audio))
        caps.append(contentsOf: parseStorage(storage))
        return caps
    }

    private static func topLevelArray(_ data: Data?, _ key: String) -> [[String: Any]] {
        guard let data,
            let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
            let items = root[key] as? [[String: Any]]
        else {
            return []
        }
        return items
    }

    private static func string(_ item: [String: Any], _ key: String) -> String? {
        if let value = item[key] as? String { return value }
        if let value = item[key] as? Int { return String(value) }
        if let value = item[key] as? Double { return String(value) }
        return nil
    }

    private static func parseDisplays(_ data: Data?) -> [HardwareCapability] {
        topLevelArray(data, "SPDisplaysDataType").map { item in
            var props: [String: String] = [:]
            for key in [
                "spdisplays_vendor", "sppci_model", "spdisplays_vram", "spdisplays_vram_shared",
            ] {
                if let value = string(item, key) { props[key] = value }
            }
            let name = string(item, "_name") ?? "GPU"
            return HardwareCapability(
                category: "gpu",
                devicePath: string(item, "sppci_bus") ?? "",
                description: name,
                properties: props
            )
        }
    }

    private static func parseUSB(_ data: Data?) -> [HardwareCapability] {
        var caps: [HardwareCapability] = []
        func walk(_ items: [[String: Any]]) {
            for item in items {
                if let children = item["_items"] as? [[String: Any]] {
                    walk(children)
                }
                // Only surface leaf devices with vendor/product identity, not
                // controllers/hubs that just carry children.
                guard string(item, "vendor_id") != nil || string(item, "product_id") != nil else {
                    continue
                }
                var props: [String: String] = [:]
                for key in [
                    "manufacturer", "vendor_id", "product_id", "location_id", "serial_num",
                    "bcd_device",
                ] {
                    if let value = string(item, key) { props[key] = value }
                }
                caps.append(
                    HardwareCapability(
                        category: "usb",
                        devicePath: string(item, "location_id") ?? "",
                        description: string(item, "_name") ?? "USB Device",
                        properties: props
                    )
                )
            }
        }
        walk(topLevelArray(data, "SPUSBDataType"))
        return caps
    }

    private static func parseCamera(_ data: Data?) -> [HardwareCapability] {
        topLevelArray(data, "SPCameraDataType").map { item in
            var props: [String: String] = [:]
            for key in ["spcamera_model-id", "spcamera_unique-id"] {
                if let value = string(item, key) { props[key] = value }
            }
            return HardwareCapability(
                category: "camera",
                devicePath: string(item, "spcamera_unique-id") ?? "",
                description: string(item, "_name") ?? "Camera",
                properties: props
            )
        }
    }

    private static func parseAudio(_ data: Data?) -> [HardwareCapability] {
        var caps: [HardwareCapability] = []
        func walk(_ items: [[String: Any]]) {
            for item in items {
                if let children = item["_items"] as? [[String: Any]] {
                    walk(children)
                }
                // Audio "devices" carry a sample-rate or input/output source key;
                // controllers just nest children.
                let isDevice =
                    item["coreaudio_device_srate"] != nil
                    || item["coreaudio_device_input"] != nil
                    || item["coreaudio_device_output"] != nil
                guard isDevice else { continue }
                var props: [String: String] = [:]
                for key in [
                    "coreaudio_device_srate", "coreaudio_device_input", "coreaudio_device_output",
                    "coreaudio_device_transport",
                ] {
                    if let value = string(item, key) { props[key] = value }
                }
                caps.append(
                    HardwareCapability(
                        category: "audio",
                        devicePath: "",
                        description: string(item, "_name") ?? "Audio Device",
                        properties: props
                    )
                )
            }
        }
        walk(topLevelArray(data, "SPAudioDataType"))
        return caps
    }

    private static func parseStorage(_ data: Data?) -> [HardwareCapability] {
        topLevelArray(data, "SPStorageDataType").map { item in
            var props: [String: String] = [:]
            for key in ["size_in_bytes", "mount_point", "file_system", "bsd_name"] {
                if let value = string(item, key) { props[key] = value }
            }
            if let physical = item["physical_drive"] as? [String: Any] {
                if let medium = string(physical, "medium_type") { props["medium_type"] = medium }
                if let proto = string(physical, "device_name") { props["device_name"] = proto }
            }
            return HardwareCapability(
                category: "storage",
                devicePath: string(item, "bsd_name") ?? "",
                description: string(item, "_name") ?? "Storage",
                properties: props
            )
        }
    }

    // MARK: - Network (live only)

    static func networkCapabilities() -> [HardwareCapability] {
        var caps: [HardwareCapability] = []
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return [] }
        defer { freeifaddrs(ifaddr) }

        var seen = Set<String>()
        for ptr in sequence(first: first, next: { $0.pointee.ifa_next }) {
            let name = String(cString: ptr.pointee.ifa_name)
            // Skip loopback and duplicate per-family entries.
            guard name != "lo0", !seen.contains(name) else { continue }
            seen.insert(name)
            caps.append(
                HardwareCapability(
                    category: "network",
                    devicePath: name,
                    description: name,
                    properties: [:]
                )
            )
        }
        return caps.sorted { $0.devicePath < $1.devicePath }
    }
}
