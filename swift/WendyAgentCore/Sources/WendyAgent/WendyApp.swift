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
    var process: Foundation.Process?
    var launchToken: UUID?

    enum CodingKeys: String, CodingKey {
        case info
        case native
        case container
    }
}
