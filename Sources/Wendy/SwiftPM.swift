import Foundation
import NIOCore
import NIOPosix
import Noora
import Subprocess
@preconcurrency import SystemPackage

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

/// Thread-safe buffer for collecting subprocess output
private actor OutputCollector {
    var output: String = ""

    func append(_ line: String) {
        output += line + "\n"
    }

    func getOutput() -> String {
        output
    }
}

/// Opens a pseudo-terminal pair for getting line-buffered output from subprocesses
/// Returns (masterFD, slaveFD) as raw Int32 values
#if !os(Windows)
    private struct PTYError: Error {
        let code: Int32
        var localizedDescription: String {
            String(cString: strerror(code))
        }
    }

    private func openPTY() throws -> (master: Int32, slave: Int32) {
        var masterFD: Int32 = -1
        var slaveFD: Int32 = -1

        #if canImport(Darwin) || canImport(Glibc) || canImport(Musl)
            guard openpty(&masterFD, &slaveFD, nil, nil, nil) == 0 else {
                throw PTYError(code: errno)
            }
        #else
            #error("PTY not supported on this platform")
        #endif

        return (masterFD, slaveFD)
    }
#endif

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

    func arguments(_ arguments: [String]) -> Subprocess.Arguments {
        // Use the executable path instead of just the command name
        let runArgs = path.split(separator: " ").dropFirst().map(String.init)

        return Subprocess.Arguments(runArgs + arguments)
    }

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

            throw SubprocessError.nonZeroExit(
                command: args.description,
                exitCode: exitCode,
                output: result.standardOutput ?? "",
                error: ""
            )
        }

        return try JSONDecoder().decode(InstalledToolchains.self, from: Data(output.utf8))
            .toolchains
    }

    public func installSDK(
        from url: String,
        checksum: String,
        onOutput: (@Sendable (String) async throws -> Void)? = nil
    ) async throws {
        let flags = ["sdk", "install", url, "--checksum", checksum]

        if let onOutput {
            // Use PTY for streaming output
            var scriptArgs = ["-q", "-F", "/dev/null"]
            scriptArgs.append(contentsOf: path.split(separator: " ").map(String.init))
            scriptArgs.append(contentsOf: flags)

            // Helper to strip ANSI escape sequences and control characters from PTY output
            func sanitizePTYOutput(_ line: String) -> String {
                var result = line
                while let escRange = result.range(
                    of: "\u{1B}\\[[0-9;]*[A-Za-z~]",
                    options: .regularExpression
                ) {
                    result.removeSubrange(escRange)
                }
                while let oscRange = result.range(
                    of: "\u{1B}\\][^\u{07}\u{1B}]*[\u{07}]",
                    options: .regularExpression
                ) {
                    result.removeSubrange(oscRange)
                }
                result = result.replacingOccurrences(of: "\r", with: "")
                return result
            }

            let result = try await Subprocess.run(
                Subprocess.Executable.name("script"),
                arguments: Subprocess.Arguments(scriptArgs)
            ) { _, stdin, stdout, stderr in
                try await stdin.finish()

                try await withThrowingTaskGroup(of: Void.self) { group in
                    group.addTask {
                        for try await line in stdout.lines() {
                            let sanitized = sanitizePTYOutput(line)
                            if !sanitized.isEmpty {
                                try await onOutput(sanitized)
                            }
                        }
                    }
                    group.addTask {
                        for try await line in stderr.lines() {
                            let sanitized = sanitizePTYOutput(line)
                            if !sanitized.isEmpty {
                                try await onOutput(sanitized)
                            }
                        }
                    }
                    try await group.waitForAll()
                }
            }

            guard result.terminationStatus.isSuccess else {
                let exitCode =
                    switch result.terminationStatus {
                    case .exited(let code), .unhandledException(let code):
                        Int(code)
                    }

                throw SubprocessError.nonZeroExit(
                    command: "script " + scriptArgs.joined(separator: " "),
                    exitCode: exitCode,
                    output: "",
                    error: ""
                )
            }
        } else {
            // Non-streaming version
            let args = arguments(flags)
            let result = try await Subprocess.run(
                .name(executableName),
                arguments: args,
                output: .string(limit: 10_000),
                error: .string(limit: 10_000)
            )

            guard result.terminationStatus.isSuccess else {
                let exitCode =
                    switch result.terminationStatus {
                    case .exited(let code), .unhandledException(let code):
                        Int(code)
                    }

                throw SubprocessError.nonZeroExit(
                    command: args.description,
                    exitCode: exitCode,
                    output: result.standardOutput ?? "",
                    error: result.standardError ?? ""
                )
            }
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

            throw SubprocessError.nonZeroExit(
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
            throw SubprocessError.nonZeroExit(
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
            throw SubprocessError.nonZeroExit(
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
        onOutput: @escaping @Sendable (String) async throws -> Void
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
            throw SubprocessError.nonZeroExit(
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
            throw SubprocessError.nonZeroExit(
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
            throw SubprocessError.nonZeroExit(
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

public func run(
    executable: Executable,
    arguments: Arguments,
    onOutput: @escaping @Sendable (String) async throws -> Void
) async throws {
    #if os(Windows)
        // Windows doesn't support PTY, fall back to regular pipes (block buffered)
        let result = try await Subprocess.run(
            executable,
            arguments: arguments,
        ) { _, stdin, stdout, stderr in
            try await stdin.finish()
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask {
                    for try await line in stdout.lines() {
                        try await onOutput(line)
                    }
                }
                group.addTask {
                    for try await line in stderr.lines() {
                        try await onOutput(line)
                    }
                }
                try await group.waitForAll()
            }
        }

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError.nonZeroExit(
                command: executableName + " " + arguments(flags).description,
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    #else
        // Use PTY for line-buffered output (subprocess sees a terminal)
        let (masterFD, slaveFD) = try openPTY()
        let fdFlags = fcntl(masterFD, F_GETFL)
        _ = fcntl(masterFD, F_SETFL, fdFlags | O_NONBLOCK)

        // Helper to read lines from PTY master using NIO
        @Sendable func readPTYLines(
            masterFD: Int32,
            eventLoopGroup: any EventLoopGroup
        ) async throws {
            let channel = try await NIOPipeBootstrap(group: eventLoopGroup)
                .channelOption(.allowRemoteHalfClosure, value: true)
                .takingOwnershipOfDescriptor(input: masterFD)
                .flatMapThrowing { channel in
                    try NIOAsyncChannel<ByteBuffer, Never>(
                        wrappingChannelSynchronously: channel
                    )
                }
                .get()

            try await channel.executeThenClose { inbound, _ in
                var buffer = ByteBuffer()
                for try await chunk in inbound {
                    buffer.writeImmutableBuffer(chunk)
                    while let newlineIndex = buffer.readableBytesView.firstIndex(
                        of: UInt8(ascii: "\n")
                    ) {
                        let lineLength = buffer.readableBytesView.distance(
                            from: buffer.readableBytesView.startIndex,
                            to: newlineIndex
                        )
                        var line = buffer.readString(length: lineLength) ?? ""
                        buffer.moveReaderIndex(forwardBy: 1)  // Skip the newline
                        // Strip ANSI escape sequences (and orphaned sequences split across chunks)
                        line.replace(
                            /\u{1B}\[[0-9;]*[A-Za-z~]|\[[0-9;]*[A-Za-z~]|\u{1B}/,
                            with: ""
                        )
                        line = line.trimmingCharacters(in: .whitespacesAndNewlines)
                        if line.isEmpty { continue }
                        try await onOutput(line)
                    }
                    buffer.discardReadBytes()
                }
            }
        }

        // Extract values before task group to avoid capturing self
        let eventLoopGroup = MultiThreadedEventLoopGroup.singleton

        // Store termination status from subprocess
        let terminationStatus = try await withThrowingTaskGroup(
            of: TerminationStatus?.self
        ) { group in
            // Reader task - NIO takes ownership of masterFD
            group.addTask {
                try await readPTYLines(masterFD: masterFD, eventLoopGroup: eventLoopGroup)
                return nil
            }

            // Subprocess task
            group.addTask {
                let result = try await Subprocess.run(
                    executable,
                    arguments: arguments,
                    output: .fileDescriptor(
                        .init(rawValue: slaveFD),
                        closeAfterSpawningProcess: true
                    ),
                    error: .fileDescriptor(
                        .init(rawValue: slaveFD),
                        closeAfterSpawningProcess: true
                    )
                )
                return result.terminationStatus
            }

            var status: TerminationStatus?
            for try await taskResult in group {
                if let terminationStatus = taskResult {
                    status = terminationStatus
                    // NIO owns the master FD and will close it when channel closes
                }
            }
            guard let status else {
                throw SubprocessError.nonZeroExit(
                    command: executable.description + " " + arguments.description,
                    exitCode: -1,
                    output: "",
                    error: "No termination status received"
                )
            }
            return status
        }

        guard terminationStatus.isSuccess else {
            let exitCode: Int
            switch terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError.nonZeroExit(
                command: executable.description + " " + arguments.description,
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    #endif
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
