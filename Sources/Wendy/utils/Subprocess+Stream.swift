import NIOCore
import NIOPosix
import Subprocess

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

/// Runs a subprocess and streams the output to the given callback.
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
                command: executable.description + " " + arguments.description,
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

/// Error thrown when a subprocess execution fails.
public enum SubprocessError: Error, CustomStringConvertible {
    case nonZeroExit(command: String, exitCode: Int, output: String, error: String)

    public var description: String {
        switch self {
        case .nonZeroExit(let command, let exitCode, let output, let error):
            return """
                Command '\(command)' failed with exit code \(exitCode): \(error)

                \(output)
                """
        }
    }
}
