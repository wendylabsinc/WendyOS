import Foundation

struct WendyApp: Codable {
    struct NativeMetadata: Codable, Equatable {
        var directory: String
        var binaryName: String
        var args: [String]
        var currentDirectory: String?
    }

    struct ContainerMetadata: Codable, Equatable {
        var imageName: String
        var appConfig: WendyAppConfig?
    }

    var info: WendyAppInfo
    var native: NativeMetadata?
    var container: ContainerMetadata?

    /// Native runtime handle; set while a darwin-native app is running.
    var process: Foundation.Process?
    /// Attached docker-run runtime handle; set while a Linux container app
    /// is running and streams are being forwarded to gRPC.
    var dockerRunTask: Task<Void, Never>?
    var launchToken: UUID?

    enum CodingKeys: String, CodingKey {
        case info
        case native
        case container
    }
}
