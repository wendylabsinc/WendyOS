import Foundation
import Noora
import Subprocess
import SystemPackage

/// Represents the Swift Package Manager interface for building and managing Swift packages.
public struct SwiftPM: Sendable {
    public let path: String

    /// Default Swift version to use for building packages
    public static let defaultSwiftVersion = "+6.2"

    /// Custom Swift version, defaults to defaultSwiftVersion if nil
    public let swiftVersion: String?

    public init(
        path: String = "swiftly run swift",
        swiftVersion: String? = SwiftPM.defaultSwiftVersion
    ) {
        self.path = path
        self.swiftVersion = swiftVersion
    }

    public enum BuildOption: Sendable {
        /// Filter for selecting a specific Swift SDK to build with.
        case swiftSDK(String)

        /// Print the binary output path
        case showBinPath

        /// Build the specified target.
        case target(String)

        /// Build the specified product.
        case product(String)

        /// `release` or `debug`
        case configuration(String)

        /// Decrease verbosity to only include error output.
        case quiet

        /// Specify a custom scratch directory path (default .build)
        case scratchPath(String)

        /// Use the static Swift standard library.
        case staticSwiftStdlib

        case disableResolution

        case xLinker(String)

        /// The arguments to pass to the Swift build command.
        var arguments: [String] {
            switch self {
            case .configuration(let configuration):
                return ["--configuration", configuration]
            case .swiftSDK(let sdk):
                return ["--swift-sdk", sdk]
            case .showBinPath:
                return ["--show-bin-path"]
            case .target(let target):
                return ["--target", target]
            case .product(let product):
                return ["--product", product]
            case .quiet:
                return ["--quiet"]
            case .scratchPath(let path):
                return ["--scratch-path", path]
            case .staticSwiftStdlib:
                return ["--static-swift-stdlib"]
            case .disableResolution:
                return ["--disable-automatic-resolution"]
            case .xLinker(let linker):
                return ["-Xlinker", linker]
            }
        }
    }

    /// Build the Swift package.
    public func buildWithOutput(_ options: BuildOption...) async throws -> String {
        let version = swiftVersion.map { [$0] } ?? []

        // Find the executable path
        let executableName = path.split(separator: " ").first.map(String.init) ?? path
        // print("Using swiftly at path: \(executablePath)")

        // Use the executable path instead of just the command name
        let runArgs = path.split(separator: " ").dropFirst().map(String.init)
        let allArgs =
            runArgs + ["build"] + version + options.flatMap(\.arguments)

        let result = try await Subprocess.run(
            .name(executableName),
            arguments: Subprocess.Arguments(allArgs),
            output: .string(limit: .max),
            error: .standardError
        )

        if result.terminationStatus.isSuccess {
            return result.standardOutput ?? ""
        } else {
            throw SubprocessError.nonZeroExit(
                command: allArgs.joined(separator: " "),
                exitCode: Int(result.terminationStatus.description) ?? -1,
                output: "",
                error: ""
            )
        }
    }

    /// Build the Swift package.
    public func build(_ options: BuildOption...) async throws {
        let version = swiftVersion.map { [$0] } ?? []

        // Find the executable path
        let executableName = path.split(separator: " ").first.map(String.init) ?? path
        // print("Using swiftly at path: \(executablePath)")

        // Use the executable path instead of just the command name
        let runArgs = path.split(separator: " ").dropFirst().map(String.init)
        let allArgs =
            runArgs + ["build"] + version + options.flatMap(\.arguments)

        let result = try await Noora().progressStep(
            message: "Building Swift package",
            successMessage: "Swift package built successfully",
            errorMessage: "Failed to build Swift package",
            showSpinner: true
        ) { _ in
            try await Subprocess.run(
                Subprocess.Executable.name(executableName),
                arguments: Subprocess.Arguments(allArgs),
                output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false), // TODO: Pipe into Noora?
                error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false), // TODO: Pipe into Noora?
            )
        }

        if result.terminationStatus.isSuccess {
            return result.standardOutput
        } else {
            throw SubprocessError.nonZeroExit(
                command: allArgs.joined(separator: " "),
                exitCode: Int(result.terminationStatus.description) ?? -1,
                output: "",
                error: ""
            )
        }
    }

    public func dumpPackage(_ options: BuildOption...) async throws -> Package {
        // Find the executable path
        let executableName = path.split(separator: " ").first.map(String.init) ?? path

        // Use the executable path instead of just the command name
        let runArgs = path.split(separator: " ").dropFirst().map(String.init)
        let allArgs =
            runArgs + ["package", "dump-package"] + options.flatMap(\.arguments)

        let result = try await Subprocess.run(
            Subprocess.Executable.name(executableName),
            arguments: Subprocess.Arguments(allArgs),
            output: .string(limit: 100_000),
            error: .string(limit: 100_000)
        )

        if result.terminationStatus.isSuccess, let output = result.standardOutput {
            return try JSONDecoder().decode(Package.self, from: Data(output.utf8))
        } else {
            throw SubprocessError.nonZeroExit(
                command: allArgs.joined(separator: " "),
                exitCode: Int(result.terminationStatus.description) ?? -1,
                output: result.standardOutput ?? "",
                error: result.standardError ?? ""
            )
        }
    }

    /// Error thrown when a subprocess execution fails.
    public enum SubprocessError: Error, LocalizedError {
        case nonZeroExit(command: String, exitCode: Int, output: String, error: String)

        public var errorDescription: String? {
            switch self {
            case .nonZeroExit(let command, let exitCode, let output, let error):
                return """
                    Command '\(command)' failed with exit code \(exitCode): \(error)

                    \(output)
                    """
            }
        }
    }

    /// The return type of the `dumpPackage` method.
    /// Currently incomplete.
    public struct Package: Decodable, Sendable {
        public var targets: [Target]

        public struct Target: Decodable, Sendable {
            public var name: String
            public var type: String
        }
    }
}
