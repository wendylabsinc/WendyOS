#if canImport(FoundationEssentials)
    import FoundationEssentials
#else
    import Foundation
#endif

public enum WendyFeature: String, Codable {
    case CUDA, ROCM
    case OCI

    // Linux Only
    case PipeWire, PulseAudio
    case NetworkManager, Connman
    case BlueZ

    static func detect() async throws -> [WendyFeature] {
        var featurset: [WendyFeature] = []

        #if os(Linux)
            if FileManager.default.fileExists(atPath: "/dev/nvidiactl") {
                featurset.append(.CUDA)
            }
            // TODO: ROCM + NetworkManager + Connman
            if FileManager.default.fileExists(atPath: "/bin/bluetoothctl") {
                featurset.append(.BlueZ)
            }
            if FileManager.default.fileExists(atPath: "/usr/bin/pipewire") {
                featurset.append(.PipeWire)
            }
            if FileManager.default.fileExists(atPath: "/usr/bin/pulseaudio") {
                featurset.append(.PulseAudio)
            }
            if FileManager.default.fileExists(atPath: "/run/containerd/containerd.sock") {
                featurset.append(.OCI)
            }
        #endif

        return featurset
    }
}
