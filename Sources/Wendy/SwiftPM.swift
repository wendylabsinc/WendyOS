import Foundation
import NIOCore
import NIOPosix
import Noora
import Subprocess
@preconcurrency import SystemPackage

/// Represents the Swift Package Manager interface for building and managing Swift packages.
public struct SwiftPM: Sendable {
    public let path: String

    /// Default Swift version to use for building packages
    public static let defaultSwiftVersion = "6.2.3"

    /// Custom Swift version, defaults to defaultSwiftVersion if nil
    public let swiftVersion: String?

    private var executableName: String {
        path.split(separator: " ").first.map(String.init) ?? path
    }

    /// Check if swiftly is available on the system.
    /// Returns true if swiftly is installed and accessible.
    public static func isSwiftlyAvailable() async -> Bool {
        do {
            let result = try await Subprocess.run(
                .name("swiftly"),
                arguments: ["--version"],
                output: .discarded,
                error: .discarded
            )
            return result.terminationStatus.isSuccess
        } catch {
            return false
        }
    }

    func arguments(_ arguments: [String]) -> Subprocess.Arguments {
        // Use the executable path instead of just the command name
        let runArgs = path.split(separator: " ").dropFirst().map(String.init)

        return Subprocess.Arguments(runArgs + arguments)
    }

    #if os(Windows)
        public init(
            path: String = "swift",
            swiftVersion: String? = SwiftPM.defaultSwiftVersion
        ) {
            self.path = path
            self.swiftVersion = swiftVersion
        }
    #else
        public init(
            path: String = "swiftly run swift",
            swiftVersion: String? = SwiftPM.defaultSwiftVersion
        ) {
            self.path = path
            self.swiftVersion = swiftVersion
        }
    #endif

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

    private struct InstalledToolchains: Sendable, Codable {
        let toolchains: [InstalledToolchain]
    }

    public struct InstalledToolchain: Sendable, Codable {
        public struct Version: Sendable, Codable {
            let major: Int?
            let minor: Int?
            let patch: Int?

            let name: String
            let type: String
        }

        let inUse: Bool
        let isDefault: Bool
        let version: Version
    }

    public func listSwiftVersions() async throws -> [InstalledToolchain] {
        #if os(Windows)
            return []
        #else
            let args = Arguments(["list", "--format", "json"])
            let result = try await Subprocess.run(
                .name("swiftly"),
                arguments: args,
                output: .string(limit: 10_000),
                error: .discarded
            )

            guard result.terminationStatus.isSuccess, let output = result.standardOutput else {
                let exitCode =
                    switch result.terminationStatus {
                    case .exited(let code), .unhandledException(let code):
                        Int(code)
                    }

                throw SubprocessError(
                    command: args.description,
                    exitCode: exitCode,
                    output: result.standardOutput ?? "",
                    error: ""
                )
            }

            return try JSONDecoder().decode(InstalledToolchains.self, from: Data(output.utf8))
                .toolchains
        #endif
    }

    public func installSDK(
        from url: String,
        checksum: String
    ) async throws {
        let flags = ["sdk", "install", url, "--checksum", checksum]
        let args = arguments(flags)
        let result = try await Subprocess.run(
            .name(executableName),
            arguments: args,
            output: .string(limit: 100_000),
            error: .string(limit: 100_000)
        )

        guard result.terminationStatus.isSuccess else {
            let exitCode =
                switch result.terminationStatus {
                case .exited(let code), .unhandledException(let code):
                    Int(code)
                }

            throw SubprocessError(
                command: args.description,
                exitCode: exitCode,
                output: result.standardOutput ?? "",
                error: result.standardError ?? ""
            )
        }
    }

    public func listSDKs() async throws -> [String] {
        let args = arguments(["sdk", "list"])
        let result = try await Subprocess.run(
            .name(executableName),
            arguments: args,
            output: .string(limit: 10_000),
            error: .discarded
        )

        guard result.terminationStatus.isSuccess, let output = result.standardOutput else {
            let exitCode =
                switch result.terminationStatus {
                case .exited(let code), .unhandledException(let code):
                    Int(code)
                }

            throw SubprocessError(
                command: args.description,
                exitCode: exitCode,
                output: result.standardOutput ?? "",
                error: ""
            )
        }

        return output.split(whereSeparator: { $0.isWhitespace || $0.isNewline }).map(String.init)
    }

    /// Build the Swift package.
    public func buildWithOutput(_ options: BuildOption...) async throws -> String {
        let version = swiftVersion.map { ["+\($0)"] } ?? []
        let allArgs = arguments(["build"] + version + options.flatMap(\.arguments))

        let result = try await Subprocess.run(
            .name(executableName),
            arguments: allArgs,
            output: .string(limit: .max),
            error: .standardError
        )

        if result.terminationStatus.isSuccess {
            return result.standardOutput ?? ""
        } else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: allArgs.description,
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    /// Build the Swift package.
    public func build(_ options: BuildOption...) async throws {
        let version = swiftVersion.map { [$0] } ?? []
        let allArgs = arguments(
            ["build"] + version + options.flatMap(\.arguments)
        )

        let result = try await Noora().progressStep(
            message: "Building Swift package",
            successMessage: "Swift package built successfully",
            errorMessage: "Failed to build Swift package",
            showSpinner: true
        ) { _ in
            try await Subprocess.run(
                Subprocess.Executable.name(executableName),
                arguments: allArgs,
                output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
                error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
            )
        }

        if result.terminationStatus.isSuccess {
            return result.standardOutput
        } else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: allArgs.description,
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    public func buildAndPushContainer(
        swiftSDK: String,
        product: Executable,
        device: String,
        entrypoint: String?,
        arguments entrypointArguments: [String],
        resources: [(source: String, destination: String)],
        onOutput: @escaping @Sendable (ByteBuffer) async throws -> Void
    ) async throws {
        var flags = [
            "package",
            "--swift-sdk=\(swiftSDK)",
            "--allow-network-connections=all",
            "build-container-image",
            "--from=swift:slim",
            "--allow-insecure-http=destination",
            "--product=\(product.name)",
            "--repository=\(device):5000/\(product.name.lowercased())",
            // TODO: Select target architecture based on target device advertisement?
            "--architecture=arm64",
        ]

        flags += resources.map { "--resources=\($0.source):\($0.destination)" }

        if let entrypoint {
            flags.append("--entrypoint=\(entrypoint)")
        }

        if !entrypointArguments.isEmpty {
            flags.append("--cmd")
            flags.append(contentsOf: entrypointArguments)
        }

        try await run(
            executable: .name(executableName),
            arguments: arguments(flags),
            onOutput: onOutput
        )
    }

    public func addDependency(url: String, from: String) async throws {
        let args = arguments([
            "package",
            "add-dependency",
            url,
            "--from",
            from,
        ])
        let result = try await Subprocess.run(
            Subprocess.Executable.name(executableName),
            arguments: args,
            output: .string(limit: 100_000),
            error: .string(limit: 100_000)
        )

        guard result.terminationStatus.isSuccess else {
            throw SubprocessError(
                command: args.description,
                exitCode: Int(result.terminationStatus.description) ?? -1,
                output: result.standardOutput ?? "",
                error: result.standardError ?? ""
            )
        }
    }

    public func showDependencies() async throws -> Dependency {
        let args = arguments(["package", "show-dependencies", "--format", "json"])
        let result = try await Subprocess.run(
            Subprocess.Executable.name(executableName),
            arguments: args,
            output: .string(limit: 1_000_000),
            error: .discarded
        )

        if result.terminationStatus.isSuccess, let output = result.standardOutput {
            return try JSONDecoder().decode(Dependency.self, from: Data(output.utf8))
        } else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: args.description,
                exitCode: exitCode,
                output: result.standardOutput ?? "",
                error: ""
            )
        }
    }

    public func showExecutables() async throws -> [Executable] {
        let args = arguments(["package", "show-executables", "--format", "json"])
        let result = try await Subprocess.run(
            Subprocess.Executable.name(executableName),
            arguments: args,
            output: .string(limit: 1_000_000),
            error: .discarded
        )

        if result.terminationStatus.isSuccess, let output = result.standardOutput {
            return try JSONDecoder().decode([Executable].self, from: Data(output.utf8))
        } else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: args.description,
                exitCode: exitCode,
                output: result.standardOutput ?? "",
                error: ""
            )
        }
    }

    public struct Executable: Codable, Sendable, Hashable, CustomStringConvertible {
        public var package: String?
        public var name: String

        public var description: String {
            return name
        }
    }

    public struct Dependency: Codable, Sendable, Hashable {
        public var identity: String
        public var name: String
        public var url: String
        public var version: String
        public var path: String
        public var dependencies: [Dependency]
    }
}
