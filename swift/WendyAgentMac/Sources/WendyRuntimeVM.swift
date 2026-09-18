import Combine
import Darwin
import Foundation
@preconcurrency import Virtualization
import os

nonisolated private let runtimeLog = Logger(
    subsystem: Bundle.main.bundleIdentifier ?? "sh.wendy.WendyAgentMac",
    category: "WendyRuntime"
)

struct WendyRuntimePaths: Equatable {
    let runtimeDirectory: URL
    let kernel: URL
    let initialRAMDisk: URL

    var dataDisk: URL { runtimeDirectory.appendingPathComponent("data.raw") }
    var consoleLog: URL { runtimeDirectory.appendingPathComponent("console.log") }
    var buildkitSocket: URL { runtimeDirectory.appendingPathComponent("buildkitd.sock") }

    static func live(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        bundle: Bundle = .main,
        fileManager: FileManager = .default,
        sourceFile: String = #filePath
    ) throws -> WendyRuntimePaths {
        let cache = try fileManager.url(
            for: .cachesDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        let runtimeDirectory =
            cache
            .appendingPathComponent("wendy", isDirectory: true)
            .appendingPathComponent("runtime", isDirectory: true)

        let architecture = runtimeArchitecture
        let resourceRoot = bundle.resourceURL ?? bundle.bundleURL
        let devRuntimeResources = URL(fileURLWithPath: sourceFile)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Resources", isDirectory: true)
            .appendingPathComponent("runtime", isDirectory: true)

        let kernel =
            environment["WENDY_RUNTIME_KERNEL"].map(URL.init(fileURLWithPath:))
            ?? firstExisting(
                [
                    resourceRoot.appendingPathComponent("runtime/vmlinuz-\(architecture)"),
                    resourceRoot.appendingPathComponent("vmlinuz-\(architecture)"),
                    devRuntimeResources.appendingPathComponent("vmlinuz-\(architecture)"),
                ],
                fileManager: fileManager
            )
        let initialRAMDisk =
            environment["WENDY_RUNTIME_INITRD"].map(URL.init(fileURLWithPath:))
            ?? firstExisting(
                [
                    resourceRoot.appendingPathComponent("runtime/initramfs-\(architecture).img"),
                    resourceRoot.appendingPathComponent("initramfs-\(architecture).img"),
                    devRuntimeResources.appendingPathComponent("initramfs-\(architecture).img"),
                ],
                fileManager: fileManager
            )

        guard let kernel, fileManager.isReadableFile(atPath: kernel.path) else {
            throw WendyRuntimeError.missingArtifact(
                "Linux kernel",
                environmentOverride: "WENDY_RUNTIME_KERNEL"
            )
        }
        guard let initialRAMDisk, fileManager.isReadableFile(atPath: initialRAMDisk.path) else {
            throw WendyRuntimeError.missingArtifact(
                "initial RAM disk",
                environmentOverride: "WENDY_RUNTIME_INITRD"
            )
        }
        return WendyRuntimePaths(
            runtimeDirectory: runtimeDirectory,
            kernel: kernel,
            initialRAMDisk: initialRAMDisk
        )
    }

    private static var runtimeArchitecture: String {
        #if arch(arm64)
            "arm64"
        #elseif arch(x86_64)
            "amd64"
        #else
            "unsupported"
        #endif
    }

    private static func firstExisting(_ candidates: [URL], fileManager: FileManager) -> URL? {
        candidates.first { fileManager.isReadableFile(atPath: $0.path) }
    }
}

enum WendyRuntimeError: LocalizedError {
    case unsupportedHost
    case missingArtifact(String, environmentOverride: String)
    case missingSocketDevice
    case unixSocketPathTooLong(String)
    case unixSocket(String, Int32)

    var errorDescription: String? {
        switch self {
        case .unsupportedHost:
            "This Mac does not support the Virtualization framework."
        case .missingArtifact(let name, let environmentOverride):
            "The Wendy runtime \(name) is not installed. Set \(environmentOverride) for a development build."
        case .missingSocketDevice:
            "The Wendy runtime VM has no virtio socket device."
        case .unixSocketPathTooLong(let path):
            "The Wendy runtime socket path is too long: \(path)"
        case .unixSocket(let operation, let code):
            "The Wendy runtime could not \(operation): \(String(cString: strerror(code)))."
        }
    }
}

@MainActor
final class WendyRuntimeVM: NSObject, ObservableObject, @preconcurrency VZVirtualMachineDelegate {
    enum State: Equatable {
        case stopped
        case starting
        case running
        case stopping
        case unavailable(String)
        case failed(String)

        var menuTitle: String {
            switch self {
            case .stopped:
                "Start Local Build Service"
            case .starting:
                "Starting Local Build Service…"
            case .running:
                "Stop Local Build Service"
            case .stopping:
                "Stopping Local Build Service…"
            case .unavailable:
                "Local Build Service Unavailable"
            case .failed:
                "Local Build Service Failed"
            }
        }

        var menuImageName: String {
            switch self {
            case .running:
                "checkmark.circle.fill"
            case .starting, .stopping:
                "clock.fill"
            case .stopped, .unavailable:
                "circle"
            case .failed:
                "exclamationmark.triangle.fill"
            }
        }

        var failureDetail: String? {
            switch self {
            case .unavailable(let detail), .failed(let detail):
                detail
            case .stopped, .starting, .running, .stopping:
                nil
            }
        }
    }

    @Published private(set) var state: State = .stopped

    private var virtualMachine: VZVirtualMachine?
    private var socketForwarders: [UnixToVirtioSocketForwarder] = []
    private var consoleHandle: FileHandle?

    /// Starts Wendy's private Linux BuildKit service on explicit user request. This does not
    /// run applications, start Wendy-Agent, advertise the Mac, or make it a deployment target.
    func start() async {
        guard state == .stopped else { return }
        guard VZVirtualMachine.isSupported else {
            state = .unavailable(WendyRuntimeError.unsupportedHost.localizedDescription)
            return
        }

        state = .starting
        do {
            let paths = try WendyRuntimePaths.live()
            try prepareRuntimeStorage(paths)
            let configuration = try makeConfiguration(paths)
            let machine = VZVirtualMachine(configuration: configuration)
            machine.delegate = self
            virtualMachine = machine

            try await start(machine)
            guard let socketDevice = machine.socketDevices.first as? VZVirtioSocketDevice else {
                throw WendyRuntimeError.missingSocketDevice
            }

            let forwarders = [
                try UnixToVirtioSocketForwarder(
                    socketURL: paths.buildkitSocket,
                    guestPort: 6237,
                    socketDevice: socketDevice
                )
            ]
            for forwarder in forwarders {
                forwarder.start()
            }
            socketForwarders = forwarders
            state = .running
            runtimeLog.notice(
                "Wendy runtime is available at \(paths.buildkitSocket.path, privacy: .public)"
            )
        } catch let error as WendyRuntimeError {
            cleanup()
            switch error {
            case .missingArtifact:
                state = .unavailable(error.localizedDescription)
            default:
                state = .failed(error.localizedDescription)
            }
            runtimeLog.error(
                "Wendy runtime did not start: \(error.localizedDescription, privacy: .public)"
            )
        } catch {
            cleanup()
            state = .failed(error.localizedDescription)
            runtimeLog.error(
                "Wendy runtime did not start: \(error.localizedDescription, privacy: .public)"
            )
        }
    }

    func stop() async {
        requestStop()

        let deadline = ContinuousClock.now + .seconds(8)
        while virtualMachine != nil && ContinuousClock.now < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
        guard virtualMachine != nil else { return }

        runtimeLog.error("Timed out waiting for the local build service to stop cleanly")
        cleanup()
        state = .stopped
    }

    func requestStop() {
        stopForwarders()
        guard let machine = virtualMachine else {
            cleanup()
            state = .stopped
            return
        }
        guard machine.canRequestStop else {
            cleanup()
            state = .stopped
            return
        }

        state = .stopping
        do {
            try machine.requestStop()
        } catch {
            runtimeLog.error(
                "Could not request a clean runtime shutdown: \(error.localizedDescription, privacy: .public)"
            )
            cleanup()
            state = .stopped
        }
    }

    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        stopForwarders()
        cleanup()
        state = .stopped
    }

    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: any Error) {
        stopForwarders()
        cleanup()
        state = .failed(error.localizedDescription)
    }

    private func start(_ machine: VZVirtualMachine) async throws {
        try await withCheckedThrowingContinuation { continuation in
            machine.start { result in continuation.resume(with: result) }
        }
    }

    private func prepareRuntimeStorage(_ paths: WendyRuntimePaths) throws {
        let fileManager = FileManager.default
        try fileManager.createDirectory(
            at: paths.runtimeDirectory,
            withIntermediateDirectories: true
        )
        guard chmod(paths.runtimeDirectory.path, S_IRWXU) == 0 else {
            throw WendyRuntimeError.unixSocket("protect the runtime directory", errno)
        }

        if !fileManager.fileExists(atPath: paths.dataDisk.path) {
            guard fileManager.createFile(atPath: paths.dataDisk.path, contents: nil) else {
                throw WendyRuntimeError.unixSocket("create the persistent data disk", errno)
            }
            let disk = try FileHandle(forWritingTo: paths.dataDisk)
            try disk.truncate(atOffset: 32 * 1024 * 1024 * 1024)
            try disk.close()
        }

        if !fileManager.fileExists(atPath: paths.consoleLog.path) {
            guard fileManager.createFile(atPath: paths.consoleLog.path, contents: nil) else {
                throw WendyRuntimeError.unixSocket("create the console log", errno)
            }
        }
        for file in [paths.dataDisk, paths.consoleLog] {
            guard chmod(file.path, S_IRUSR | S_IWUSR) == 0 else {
                throw WendyRuntimeError.unixSocket("protect \(file.lastPathComponent)", errno)
            }
        }
    }

    private func makeConfiguration(
        _ paths: WendyRuntimePaths
    ) throws -> VZVirtualMachineConfiguration {
        let configuration = VZVirtualMachineConfiguration()

        let bootLoader = VZLinuxBootLoader(kernelURL: paths.kernel)
        bootLoader.initialRamdiskURL = paths.initialRAMDisk
        bootLoader.commandLine = "console=hvc0 panic=-1"
        configuration.bootLoader = bootLoader

        configuration.cpuCount = min(max(ProcessInfo.processInfo.processorCount / 2, 2), 8)
        configuration.memorySize = min(
            max(ProcessInfo.processInfo.physicalMemory / 4, 2 * 1024 * 1024 * 1024),
            8 * 1024 * 1024 * 1024
        )
        configuration.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        configuration.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        configuration.socketDevices = [VZVirtioSocketDeviceConfiguration()]

        let network = VZVirtioNetworkDeviceConfiguration()
        network.attachment = VZNATNetworkDeviceAttachment()
        configuration.networkDevices = [network]

        let diskAttachment = try VZDiskImageStorageDeviceAttachment(
            url: paths.dataDisk,
            readOnly: false
        )
        configuration.storageDevices = [
            VZVirtioBlockDeviceConfiguration(attachment: diskAttachment)
        ]

        let console = try FileHandle(forUpdating: paths.consoleLog)
        try console.seekToEnd()
        consoleHandle = console
        let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
        serial.attachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: console,
            fileHandleForWriting: console
        )
        configuration.serialPorts = [serial]

        try configuration.validate()
        return configuration
    }

    private func stopForwarders() {
        for forwarder in socketForwarders {
            forwarder.stop()
        }
        socketForwarders.removeAll()
    }

    private func cleanup() {
        virtualMachine = nil
        try? consoleHandle?.close()
        consoleHandle = nil
    }
}

/// A host-owned Unix socket that opens one guest vsock connection per accepted
/// client. It deliberately carries raw byte streams: BuildKit and containerd
/// continue to speak their native protocols end-to-end.
nonisolated private final class UnixToVirtioSocketForwarder: @unchecked Sendable {
    private let socketURL: URL
    private let guestPort: UInt32
    private let socketDevice: VZVirtioSocketDevice
    private let queue: DispatchQueue
    private var listener: Int32 = -1
    private var source: (any DispatchSourceRead)?

    init(socketURL: URL, guestPort: UInt32, socketDevice: VZVirtioSocketDevice) throws {
        self.socketURL = socketURL
        self.guestPort = guestPort
        self.socketDevice = socketDevice
        queue = DispatchQueue(label: "sh.wendy.runtime.socket.\(guestPort)")
        listener = try Self.makeListener(at: socketURL)
    }

    deinit { stop() }

    func start() {
        queue.sync {
            guard source == nil, listener >= 0 else { return }
            let readSource = DispatchSource.makeReadSource(fileDescriptor: listener, queue: queue)
            readSource.setEventHandler { [weak self] in self?.acceptAvailableClients() }
            source = readSource
            readSource.resume()
        }
    }

    func stop() {
        queue.sync {
            source?.cancel()
            source = nil
            if listener >= 0 {
                Darwin.close(listener)
                listener = -1
            }
            unlink(socketURL.path)
        }
    }

    private func acceptAvailableClients() {
        while listener >= 0 {
            let client = Darwin.accept(listener, nil, nil)
            if client < 0 {
                if errno == EINTR { continue }
                if errno != EAGAIN && errno != EWOULDBLOCK {
                    runtimeLog.error(
                        "Accept failed for \(self.socketURL.path, privacy: .public): \(errno)"
                    )
                }
                return
            }
            DispatchQueue.main.async { [socketDevice, guestPort] in
                socketDevice.connect(toPort: guestPort) { result in
                    switch result {
                    case .success(let connection):
                        DuplexFileDescriptorBridge(client: client, guest: connection).start()
                    case .failure(let error):
                        runtimeLog.error(
                            "Guest vsock \(guestPort) connect failed: \(error.localizedDescription, privacy: .public)"
                        )
                        Darwin.close(client)
                    }
                }
            }
        }
    }

    private static func makeListener(at socketURL: URL) throws -> Int32 {
        let path = socketURL.path
        let bytes = path.utf8CString
        var address = sockaddr_un()
        guard bytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
            throw WendyRuntimeError.unixSocketPathTooLong(path)
        }

        let descriptor = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard descriptor >= 0 else {
            throw WendyRuntimeError.unixSocket("open a Unix socket", errno)
        }

        address.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &address.sun_path) { target in
            for (index, byte) in bytes.enumerated() {
                target[index] = UInt8(bitPattern: byte)
            }
        }
        unlink(path)

        let bindResult = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bindResult == 0 else {
            let code = errno
            Darwin.close(descriptor)
            throw WendyRuntimeError.unixSocket("bind \(path)", code)
        }
        guard Darwin.listen(descriptor, 32) == 0 else {
            let code = errno
            Darwin.close(descriptor)
            unlink(path)
            throw WendyRuntimeError.unixSocket("listen on \(path)", code)
        }
        guard chmod(path, S_IRUSR | S_IWUSR) == 0 else {
            let code = errno
            Darwin.close(descriptor)
            unlink(path)
            throw WendyRuntimeError.unixSocket("protect \(path)", code)
        }
        let flags = fcntl(descriptor, F_GETFL)
        _ = fcntl(descriptor, F_SETFL, flags | O_NONBLOCK)
        return descriptor
    }
}

nonisolated private final class DuplexFileDescriptorBridge: @unchecked Sendable {
    private let client: Int32
    private let guest: VZVirtioSocketConnection
    private let group = DispatchGroup()
    private let copyQueue = DispatchQueue(label: "sh.wendy.runtime.bridge", attributes: .concurrent)

    init(client: Int32, guest: VZVirtioSocketConnection) {
        self.client = client
        self.guest = guest
    }

    func start() {
        let guestDescriptor = guest.fileDescriptor
        Self.configure(client)
        Self.configure(guestDescriptor)
        copy(from: client, to: guestDescriptor)
        copy(from: guestDescriptor, to: client)
        group.notify(queue: copyQueue) { [self] in
            Darwin.close(client)
            guest.close()
        }
    }

    private static func configure(_ descriptor: Int32) {
        let flags = fcntl(descriptor, F_GETFL)
        if flags >= 0 {
            _ = fcntl(descriptor, F_SETFL, flags & ~O_NONBLOCK)
        }
        var enabled: Int32 = 1
        _ = setsockopt(
            descriptor,
            SOL_SOCKET,
            SO_NOSIGPIPE,
            &enabled,
            socklen_t(MemoryLayout<Int32>.size)
        )
    }

    private func copy(from source: Int32, to destination: Int32) {
        group.enter()
        copyQueue.async { [self] in
            defer {
                _ = Darwin.shutdown(destination, SHUT_WR)
                group.leave()
            }
            var buffer = [UInt8](repeating: 0, count: 64 * 1024)
            while true {
                let count = Darwin.read(source, &buffer, buffer.count)
                if count == 0 { return }
                if count < 0 {
                    if errno == EINTR { continue }
                    return
                }
                var written = 0
                while written < count {
                    let result = buffer.withUnsafeBytes { bytes in
                        Darwin.write(
                            destination,
                            bytes.baseAddress!.advanced(by: written),
                            count - written
                        )
                    }
                    if result < 0 {
                        if errno == EINTR { continue }
                        return
                    }
                    written += result
                }
            }
        }
    }
}
