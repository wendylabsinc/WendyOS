import ArgumentParser
import CLIOutput
import Foundation
import WendyShared

struct InfoCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "info",
        abstract: "Display CLI metadata and configuration",
        discussion: """
            Outputs metadata about the Wendy CLI in JSON format, including version information,
            Swift toolchain requirements, and SDK details. Useful for IDE integrations and tooling.
            """,
        shouldDisplay: false
    )

    func run() async throws {
        let metadata = CLIMetadata(
            version: Version.current,
            swift: .init(
                version: SwiftPM.defaultSwiftVersion,
                sdk: "\(SwiftPM.defaultSwiftVersion)-RELEASE_wendyos_aarch64",
                sdkDownloadURL:
                    "https://github.com/wendylabsinc/wendy-swift-tools/releases/download/0.4.0/\(SwiftPM.defaultSwiftVersion)-RELEASE_wendyos_aarch64.artifactbundle.zip"
            )
        )

        cliOutput.result(metadata)
    }
}

/// Metadata about the Wendy CLI for consumption by IDEs and tooling.
struct CLIMetadata: Codable, Sendable {
    /// The version of the Wendy CLI
    let version: String

    /// Swift toolchain requirements
    let swift: SwiftRequirements

    struct SwiftRequirements: Codable, Sendable {
        /// The required Swift version
        let version: String

        /// The Swift SDK identifier for cross-compilation
        let sdk: String

        /// URL to download the SDK artifact bundle
        let sdkDownloadURL: String
    }
}
