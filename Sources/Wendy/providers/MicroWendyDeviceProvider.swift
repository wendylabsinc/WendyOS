import CLIOutput
import Foundation
import Hummingbird
import Logging
import NIOCore
import Subprocess
import WendyShared

#if os(macOS)
    import System
#else
    import SystemPackage
#endif

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

/// Build context for the Micro Wendy WASM provider
struct MicroWendyBuildContext: Sendable {
    let wasmPath: String
}

/// Device provider for micro-Wendy (ESP32) devices.
///
/// Builds Swift projects to WASM and serves the binary over HTTP so that
/// micro-Wendy devices on the local network can discover and download it
/// via mDNS (`_wendy._tcp`). Sends a UDP `WENDY_RELOAD` broadcast to
/// trigger connected devices to re-download.
struct MicroWendyDeviceProvider: DeviceProvider, Sendable {
    let key = "microwasm"
    let displayName = "Micro Wendy (WASM)"

    private let logger = Logger(label: "sh.wendy.provider.microwasm")

    /// Swift version required for WASM target support
    private let swiftVersion = "6.2.3"

    /// UDP reload port matching the firmware default
    private let udpReloadPort: UInt16 = 4210

    // MARK: - Availability

    func isAvailable() async -> Bool {
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

    // MARK: - Requirements

    func checkRequirements(shouldAutoAccept: Bool) async throws {
        guard await isAvailable() else {
            cliOutput.error(
                """
                swiftly is not installed. It is needed to manage Swift toolchains for WASM.

                Install it from: https://swiftlang.github.io/swiftly/
                """
            )
            throw CLIError.serviceNotInstalled(name: "swiftly")
        }

        // Check that the required Swift version is installed
        let swiftPM = SwiftPM()
        let installedVersions = try await cliOutput.withProgress(
            message: "Checking Swift requirements for WASM",
            successMessage: "Swift environment ready",
            errorMessage: "Failed to check Swift requirements"
        ) {
            try await swiftPM.listSwiftVersions()
        }

        if !installedVersions.contains(where: { $0.version.name == swiftVersion }) {
            let install: Bool

            if shouldAutoAccept {
                install = true
            } else {
                install = try await cliOutput.yesOrNoPrompt(
                    question:
                        "Swift \(swiftVersion) is required for WASM compilation. Do you want to install it?",
                    defaultAnswer: true
                )
            }

            if install {
                cliOutput.info("Installing Swift \(swiftVersion)...")
                let result = try await Subprocess.run(
                    .name("swiftly"),
                    arguments: ["install", swiftVersion],
                    output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
                    error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
                )

                guard result.terminationStatus.isSuccess else {
                    throw CLIError.commandFailed(
                        command: "swiftly install \(swiftVersion)",
                        exitCode: result.terminationStatus.wasmExitCode,
                        output: "Failed to install Swift \(swiftVersion)"
                    )
                }
                cliOutput.success("Swift \(swiftVersion) installed")
            } else {
                throw CLIError.serviceNotInstalled(
                    name: "Swift \(swiftVersion) (required for WASM)"
                )
            }
        }
    }

    // MARK: - Discovery

    func discoverDevices() async throws -> [ExternalDevice] {
        // Micro-Wendy devices discover the dev server via mDNS, not the
        // other way around. Return a single entry representing any device
        // on the LAN that supports _wendy._tcp.
        [
            ExternalDevice(
                id: "microwasm",
                displayName: "Micro Wendy (WASM over WiFi)",
                providerKey: key,
                os: "uwasm",
                cpuArchitecture: "wasm32"
            )
        ]
    }

    // MARK: - Build

    func canBuild(projectPath: URL) async -> Bool {
        FileManager.default.fileExists(
            atPath: projectPath.appendingPathComponent("Package.swift").path
        )
    }

    func build(
        for device: ExternalDevice,
        projectPath: URL,
        product: String,
        debug: Bool
    ) async throws -> ProviderBuiltApp {
        cliOutput.info("Building \(product) for WASM (wasm32-unknown-none-wasm)...")

        let result = try await Subprocess.run(
            .name("swiftly"),
            arguments: Arguments([
                "run", "+6.2.3",
                "swift", "build",
                "--triple", "wasm32-unknown-none-wasm",
                "-c", debug ? "debug" : "release",
            ]),
            workingDirectory: FilePath(projectPath.path),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            throw CLIError.commandFailed(
                command: "swift build --triple wasm32-unknown-none-wasm",
                exitCode: result.terminationStatus.wasmExitCode,
                output: "WASM build failed"
            )
        }

        let configuration = debug ? "debug" : "release"
        let wasmPath =
            projectPath
            .appendingPathComponent(".build/\(configuration)/\(product).wasm")
            .path

        guard FileManager.default.fileExists(atPath: wasmPath) else {
            throw CLIError.fileNotFound(path: wasmPath)
        }

        let fileSize =
            (try? FileManager.default.attributesOfItem(atPath: wasmPath)[.size] as? Int) ?? 0
        cliOutput.success("Built \(product).wasm (\(fileSize) bytes)")

        return ProviderBuiltApp(
            provider: self,
            device: device,
            appName: product,
            context: MicroWendyBuildContext(wasmPath: wasmPath)
        )
    }

    // MARK: - Run

    func run(
        _ builtApp: ProviderBuiltApp,
        detach: Bool,
        output: AsyncStream<ProviderRunOutput>.Continuation
    ) async throws {
        guard let ctx = builtApp.context as? MicroWendyBuildContext else {
            throw CLIError.invalidArgument(
                name: "context",
                value: "unknown",
                reason: "Invalid build context for Micro Wendy provider"
            )
        }

        // Read WASM binary into a buffer served by the HTTP endpoint
        let wasmData = try Data(contentsOf: URL(fileURLWithPath: ctx.wasmPath))
        let wasmBuffer = ByteBuffer(data: wasmData)

        // Build Hummingbird router with a single endpoint
        let router = Router()
        router.get("/app.wasm") { _, _ in
            Response(
                status: .ok,
                headers: [.contentType: "application/wasm"],
                body: .init(byteBuffer: wasmBuffer)
            )
        }

        // Use onServerRunning to capture the dynamically assigned port
        let (portStream, portContinuation) = AsyncStream<Int>.makeStream()

        let app = Application(
            router: router,
            configuration: .init(address: .hostname("0.0.0.0", port: 0)),
            onServerRunning: { channel in
                if let port = channel.localAddress?.port {
                    portContinuation.yield(port)
                }
                portContinuation.finish()
            }
        )

        try await withThrowingTaskGroup(of: Void.self) { group in
            // 1. Run the HTTP server (blocks until graceful shutdown)
            group.addTask {
                try await app.runService()
            }

            // 2. Wait for server to bind, register mDNS, send reload, signal started
            group.addTask { [self] in
                var serverPort = 8080
                for await port in portStream {
                    serverPort = port
                }

                cliOutput.info(
                    "Serving \(ctx.wasmPath) at http://0.0.0.0:\(serverPort)/app.wasm"
                )

                // Register mDNS so micro-Wendy devices can discover us
                let mdnsProcess = startMDNSRegistration(port: serverPort)
                defer {
                    mdnsProcess?.terminate()
                    mdnsProcess?.waitUntilExit()
                }

                // Notify connected devices to re-download
                sendReloadBroadcast(port: udpReloadPort)

                output.yield(.started)

                // Keep this task alive until cancelled
                while !Task.isCancelled {
                    try await Task.sleep(for: .seconds(3600))
                }
            }

            try await group.next()
            group.cancelAll()
        }

        output.finish()
    }

    // MARK: - Stop

    func stop(_ builtApp: ProviderBuiltApp) async throws {
        // Cleanup is handled by task cancellation in run()
    }

    // MARK: - mDNS Registration

    /// Start an mDNS registration process advertising `_wendy._tcp` on the given port.
    /// Returns the running Process handle, or nil if registration failed.
    private func startMDNSRegistration(port: Int) -> Process? {
        let process = Process()
        #if os(macOS)
            process.executableURL = URL(fileURLWithPath: "/usr/bin/dns-sd")
            process.arguments = [
                "-R", "Wendy Dev Server", "_wendy._tcp", "local.", String(port),
            ]
        #else
            process.executableURL = URL(fileURLWithPath: "/usr/bin/avahi-publish-service")
            process.arguments = [
                "Wendy Dev Server", "_wendy._tcp", String(port),
            ]
        #endif
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice

        do {
            try process.run()
            cliOutput.info("Registered mDNS: _wendy._tcp on port \(port)")
            return process
        } catch {
            cliOutput.warning(
                "Failed to register mDNS service (dns-sd/avahi not available): \(error)"
            )
            return nil
        }
    }

    // MARK: - UDP Broadcast

    /// Send a WENDY_RELOAD UDP broadcast so devices re-download the WASM binary.
    private func sendReloadBroadcast(port: UInt16) {
        let sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP)
        guard sock >= 0 else {
            cliOutput.warning("Failed to create UDP socket for reload broadcast")
            return
        }
        defer { close(sock) }

        var broadcastEnable: Int32 = 1
        setsockopt(
            sock, SOL_SOCKET, SO_BROADCAST,
            &broadcastEnable, socklen_t(MemoryLayout<Int32>.size)
        )

        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = port.bigEndian
        addr.sin_addr.s_addr = INADDR_BROADCAST

        let message = "WENDY_RELOAD"
        message.withCString { cstr in
            withUnsafePointer(to: &addr) { addrPtr in
                addrPtr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockAddrPtr in
                    _ = sendto(
                        sock, cstr, strlen(cstr), 0,
                        sockAddrPtr, socklen_t(MemoryLayout<sockaddr_in>.size)
                    )
                }
            }
        }

        cliOutput.info("Sent WENDY_RELOAD broadcast on UDP port \(port)")
    }
}

// MARK: - Helpers

extension TerminationStatus {
    fileprivate var wasmExitCode: Int32 {
        switch self {
        case .exited(let code), .unhandledException(let code):
            return code
        }
    }
}
