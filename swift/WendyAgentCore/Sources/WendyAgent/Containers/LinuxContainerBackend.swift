import Foundation

/// Runtime-neutral description of one container run flag, interpreted from a
/// Wendy entitlement. Each concrete backend renders these to its own CLI flags.
enum LinuxRunSpec: Equatable, Sendable {
    case networkNone
    case publishPort(host: UInt16, container: UInt16)
    case volume(name: String, path: String)
}

/// A Wendy-managed container as reported by a runtime's list command.
struct LinuxContainerInfo: Sendable, Equatable {
    let id: String
    let name: String
    let state: String
}

/// The managed container name for an app (`wendy-<appName>`).
func managedContainerName(for appName: String) -> String { "wendy-\(appName)" }

/// A Linux-container runtime the Mac agent can drive (Apple `container` or Docker).
protocol LinuxContainerBackend: Sendable {
    func pull(image: String) async throws
    func createAndStart(
        appName: String,
        imageName: String,
        appConfig: WendyAppConfig?,
        terminationHandler: (@Sendable (Foundation.Process) -> Void)?
    ) async throws -> (process: Foundation.Process, stdout: Pipe, stderr: Pipe)
    func stop(appName: String) async throws
    func remove(appName: String) async throws
    func listContainers() async throws -> [LinuxContainerInfo]
}

/// Interprets entitlements into runtime-neutral run specs. Single source of
/// truth shared by every backend so mapping stays consistent.
enum LinuxRunSpecBuilder {
    /// Hardware entitlements that VM-isolated Linux containers can't honor on macOS.
    static let unsupportedHardwareTypes: Set<String> = [
        "gpu", "npu", "bluetooth", "audio", "video", "camera", "usb", "i2c", "gpio",
    ]

    static func specs(
        from entitlements: [WendyEntitlement],
        appName: String,
        warn: (String) -> Void
    ) -> [LinuxRunSpec] {
        var specs: [LinuxRunSpec] = []
        for entitlement in entitlements {
            switch entitlement.type {
            case "network":
                if entitlement.mode == "none" {
                    specs.append(.networkNone)
                } else if let ports = entitlement.ports {
                    for port in ports {
                        specs.append(.publishPort(host: port.host, container: port.container))
                    }
                }
            case "persist":
                if let name = entitlement.name, let path = entitlement.path {
                    specs.append(.volume(name: "wendy-\(appName)-\(name)", path: path))
                }
            case "http", "mcp":
                // Metadata-only entitlements: the port is read directly from the
                // app's retained WendyAppConfig at listContainers time (see
                // ContainerService.listContainers), not applied to the container's
                // run spec. Mirrors the Go agent, where ApplyEntitlements has no
                // case for either type either.
                break
            case let type where unsupportedHardwareTypes.contains(type):
                warn(
                    "Entitlement '\(type)' is not available for Linux containers on macOS (VM isolation)"
                )
            default:
                warn("Unknown entitlement type '\(entitlement.type)'")
            }
        }
        return specs
    }
}
