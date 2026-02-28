import Foundation
import Logging
import Subprocess

/// Manages Bluetooth operations using BlueZ (bluetoothctl)
actor BluetoothManager {
    let logger: Logger
    private static let defaultScanTimeoutSeconds: UInt32 = 20

    /// Current scan task, if any
    private var scanTask: Task<Void, Never>?
    /// Current timeout task, if any
    private var scanTimeoutTask: Task<Void, Never>?
    /// Monotonic scan generation used to ignore stale timeouts
    private var scanGeneration: UInt64 = 0

    init(logger: Logger) {
        self.logger = logger
    }

    struct BluetoothDeviceInfo: Sendable, Equatable, Comparable {
        let name: String
        let address: String
        let rssi: Int?
        let paired: Bool
        let connected: Bool
        let trusted: Bool
        let deviceType: String
        let icon: String?

        static func < (lhs: BluetoothDeviceInfo, rhs: BluetoothDeviceInfo) -> Bool {
            // Sort by connection status, then RSSI, then name
            if lhs.connected != rhs.connected {
                return lhs.connected && !rhs.connected
            }
            if let lhsRssi = lhs.rssi, let rhsRssi = rhs.rssi, lhsRssi != rhsRssi {
                return lhsRssi > rhsRssi
            }
            return lhs.name < rhs.name
        }
    }

    struct ScanStartResult: Sendable {
        let restartedExistingScan: Bool
        let superseded: Bool
    }

    struct ScanStopResult: Sendable {
        let hadActiveScan: Bool
        let superseded: Bool
    }

    /// Run loop for managing Bluetooth operations in a structured way.
    /// Call this from a task group to enable proper lifecycle management.
    /// The run loop continues until cancelled.
    func run() async throws {
        // The run loop keeps the actor alive and allows for structured concurrency.
        // When cancelled, any active scan task will be cancelled as well.
        defer {
            scanTask?.cancel()
            scanTask = nil
        }

        // Wait indefinitely until cancelled
        try await Task.sleep(for: .seconds(Int64.max))
    }

    /// Lists Bluetooth devices using bluetoothctl
    func listDevices(pairedOnly: Bool) async throws -> [BluetoothDeviceInfo] {
        // First, get the list of device addresses
        let devicesOutput = try await runBluetoothctl(["devices"])
        let deviceAddresses = Self.parseDeviceAddresses(from: devicesOutput)

        var devices: [BluetoothDeviceInfo] = []

        // Get detailed info for each device
        for address in deviceAddresses {
            if let deviceInfo = try await getDeviceInfo(address: address) {
                if pairedOnly && !deviceInfo.paired {
                    continue
                }
                devices.append(deviceInfo)
            }
        }

        return devices
    }

    /// Gets detailed information about a specific device
    private func getDeviceInfo(address: String) async throws -> BluetoothDeviceInfo? {
        let infoOutput = try await runBluetoothctl(["info", address])
        return Self.parseDeviceInfo(from: infoOutput, address: address)
    }

    /// Starts Bluetooth discovery scan
    func startScan(timeoutSeconds: UInt32) async throws -> ScanStartResult {
        // Power on the adapter first
        try await powerOn()

        // Invalidate any in-flight timeout and scan tasks
        scanGeneration &+= 1
        let scanID = scanGeneration
        cancelTimeoutTask()

        let hadExistingScan = scanTask != nil
        let priorScanTask = scanTask
        scanTask = nil
        priorScanTask?.cancel()
        if let priorScanTask {
            logger.warning("Bluetooth scan already running; restarting scan")
            await priorScanTask.value
        }

        // If another start/stop happened while we awaited, skip starting a stale scan
        guard scanID == scanGeneration else {
            logger.debug(
                "Bluetooth scan start superseded by newer request",
                metadata: [
                    "scan_id": "\(scanID)",
                    "current_scan_id": "\(scanGeneration)",
                ]
            )
            return ScanStartResult(
                restartedExistingScan: hadExistingScan,
                superseded: true
            )
        }

        // bluetoothctl scan needs a timeout in non-interactive mode to actually discover devices.
        let effectiveTimeoutSeconds =
            timeoutSeconds == 0 ? Self.defaultScanTimeoutSeconds : timeoutSeconds

        // Start scan as a child task (structured concurrency)
        scanTask = Task { [logger] in
            do {
                let output = try await self.runBluetoothctl([
                    "--timeout", "\(effectiveTimeoutSeconds)",
                    "scan", "on",
                ])

                if !output.contains("Discovery started") {
                    logger.warning(
                        "bluetoothctl scan did not report discovery start",
                        metadata: ["output": "\(output)"]
                    )
                }
            } catch is CancellationError {
                // Expected when scan is stopped early
                logger.debug("Bluetooth scan cancelled")
            } catch {
                logger.warning(
                    "bluetoothctl scan failed",
                    metadata: ["error": "\(error)"]
                )
            }
        }

        // If a timeout is specified, schedule stopping the scan
        if timeoutSeconds > 0 {
            scanTimeoutTask = Task { [logger] in
                do {
                    try await Task.sleep(for: .seconds(Int(timeoutSeconds)))
                    await self.stopScanIfCurrent(scanID: scanID)
                } catch is CancellationError {
                    // Expected if stopped before timeout
                } catch {
                    logger.warning(
                        "Failed to stop bluetooth scan after timeout",
                        metadata: ["error": "\(error)"]
                    )
                }
            }
        } else {
            scanTimeoutTask = nil
        }

        return ScanStartResult(
            restartedExistingScan: hadExistingScan,
            superseded: false
        )
    }

    /// Stops Bluetooth discovery scan
    func stopScan() async throws -> ScanStopResult {
        try await stopScan(reason: .external)
    }

    private enum StopReason {
        case external
        case timeout
    }

    private func stopScan(reason: StopReason) async throws -> ScanStopResult {
        scanGeneration &+= 1
        let stopID = scanGeneration

        if reason == .external {
            cancelTimeoutTask()
        } else {
            scanTimeoutTask = nil
        }

        let hadActiveScan = scanTask != nil
        let priorScanTask = scanTask
        scanTask = nil
        priorScanTask?.cancel()
        if let priorScanTask {
            await priorScanTask.value
        }

        // If another scan started while we awaited, do not shut it off
        guard stopID == scanGeneration else {
            logger.debug(
                "Bluetooth scan stop superseded by newer request",
                metadata: [
                    "stop_id": "\(stopID)",
                    "current_scan_id": "\(scanGeneration)",
                ]
            )
            return ScanStopResult(
                hadActiveScan: hadActiveScan,
                superseded: true
            )
        }

        if !hadActiveScan {
            logger.debug("Stop scan requested without an active scan")
        }

        // Then tell bluetoothctl to stop scanning
        _ = try await runBluetoothctl(["scan", "off"])

        return ScanStopResult(
            hadActiveScan: hadActiveScan,
            superseded: false
        )
    }

    private func stopScanIfCurrent(scanID: UInt64) async {
        guard scanID == scanGeneration else {
            logger.debug(
                "Timeout stop ignored due to newer scan",
                metadata: [
                    "timeout_scan_id": "\(scanID)",
                    "current_scan_id": "\(scanGeneration)",
                ]
            )
            return
        }

        do {
            _ = try await stopScan(reason: .timeout)
        } catch {
            logger.warning(
                "Failed to stop bluetooth scan after timeout",
                metadata: ["error": "\(error)"]
            )
        }
    }

    private func cancelTimeoutTask() {
        scanTimeoutTask?.cancel()
        scanTimeoutTask = nil
    }

    /// Connects to a Bluetooth device by MAC address
    func connect(address: String) async throws {
        // First ensure the adapter is powered on
        try await powerOn()

        // Attempt to connect
        let output = try await runBluetoothctl(["connect", address])

        // Check if connection was successful
        if output.lowercased().contains("failed") || output.lowercased().contains("error") {
            throw BluetoothError.connectionFailed(address, output)
        }
    }

    /// Disconnects from a Bluetooth device by MAC address
    func disconnect(address: String) async throws {
        let output = try await runBluetoothctl(["disconnect", address])

        if output.lowercased().contains("failed") || output.lowercased().contains("error") {
            throw BluetoothError.disconnectionFailed(address, output)
        }
    }

    /// Pairs with a Bluetooth device by MAC address
    func pair(address: String) async throws {
        try await powerOn()
        let output = try await runBluetoothctl(["pair", address])

        if output.lowercased().contains("failed") || output.lowercased().contains("error") {
            throw BluetoothError.pairFailed(address, output)
        }
    }

    /// Trusts a Bluetooth device by MAC address
    func trust(address: String) async throws {
        try await powerOn()
        let output = try await runBluetoothctl(["trust", address])

        if output.lowercased().contains("failed") || output.lowercased().contains("error") {
            throw BluetoothError.trustFailed(address, output)
        }
    }

    /// Forgets (removes) a Bluetooth device by MAC address
    func forget(address: String) async throws {
        let output = try await runBluetoothctl(["remove", address])

        if output.lowercased().contains("failed") || output.lowercased().contains("error") {
            throw BluetoothError.forgetFailed(address, output)
        }
    }

    // MARK: - Private Helpers

    private func runBluetoothctl(_ arguments: [String]) async throws -> String {
        let result = try await Subprocess.run(
            Subprocess.Executable.name("bluetoothctl"),
            arguments: Subprocess.Arguments(arguments),
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        guard result.terminationStatus.isSuccess else {
            let stdout = (result.standardOutput ?? "").trimmingCharacters(
                in: .whitespacesAndNewlines
            )
            let stderr = (result.standardError ?? "").trimmingCharacters(
                in: .whitespacesAndNewlines
            )
            var messageParts: [String] = []
            if !stderr.isEmpty {
                messageParts.append("stderr: \(stderr)")
            }
            if !stdout.isEmpty {
                messageParts.append("stdout: \(stdout)")
            }
            let message =
                messageParts.isEmpty ? "Unknown error" : messageParts.joined(separator: "\n")
            logger.error(
                "bluetoothctl failed",
                metadata: [
                    "arguments": "\(arguments)",
                    "stderr": "\(stderr)",
                    "stdout": "\(stdout)",
                ]
            )
            throw BluetoothError.commandFailed(message)
        }

        return result.standardOutput ?? ""
    }

    private func powerOn() async throws {
        _ = try await runBluetoothctl(["power", "on"])
    }

    /// Parses device addresses from "bluetoothctl devices" output
    /// Format: "Device XX:XX:XX:XX:XX:XX DeviceName"
    static func parseDeviceAddresses(from output: String) -> [String] {
        var addresses: [String] = []
        let lines = output.components(separatedBy: .newlines)

        for line in lines {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("Device ") {
                let parts = trimmed.components(separatedBy: " ")
                if parts.count >= 2 {
                    addresses.append(parts[1])
                }
            }
        }

        return addresses
    }

    /// Parses device info from "bluetoothctl info <address>" output
    static func parseDeviceInfo(from output: String, address: String) -> BluetoothDeviceInfo? {
        var name = "Unknown"
        var rssi: Int?
        var paired = false
        var connected = false
        var trusted = false
        var deviceType = ""
        var icon: String?

        let lines = output.components(separatedBy: .newlines)

        for line in lines {
            let trimmed = line.trimmingCharacters(in: .whitespaces)

            if trimmed.hasPrefix("Name:") {
                name = String(trimmed.dropFirst(5)).trimmingCharacters(in: .whitespaces)
            } else if trimmed.hasPrefix("Alias:") && name == "Unknown" {
                name = String(trimmed.dropFirst(6)).trimmingCharacters(in: .whitespaces)
            } else if trimmed.hasPrefix("Paired:") {
                paired = trimmed.contains("yes")
            } else if trimmed.hasPrefix("Connected:") {
                connected = trimmed.contains("yes")
            } else if trimmed.hasPrefix("Trusted:") {
                trusted = trimmed.contains("yes")
            } else if trimmed.hasPrefix("Icon:") {
                icon = String(trimmed.dropFirst(5)).trimmingCharacters(in: .whitespaces)
                // Icon hints at device type
                if deviceType.isEmpty {
                    deviceType = icon ?? ""
                }
            } else if trimmed.hasPrefix("Class:") {
                // Parse Bluetooth device class if available
                // This is a hex value that indicates the device type
            } else if trimmed.hasPrefix("RSSI:") {
                // Parse RSSI value (e.g., "RSSI: -45")
                let rssiStr = String(trimmed.dropFirst(5)).trimmingCharacters(in: .whitespaces)
                rssi = Int(rssiStr)
            }
        }

        return BluetoothDeviceInfo(
            name: name,
            address: address,
            rssi: rssi,
            paired: paired,
            connected: connected,
            trusted: trusted,
            deviceType: deviceType,
            icon: icon
        )
    }
}

enum BluetoothError: Error, LocalizedError {
    case commandFailed(String)
    case deviceNotFound(String)
    case connectionFailed(String, String)
    case disconnectionFailed(String, String)
    case pairFailed(String, String)
    case trustFailed(String, String)
    case forgetFailed(String, String)

    var errorDescription: String? {
        switch self {
        case .commandFailed(let message):
            return "Bluetooth command failed: \(message)"
        case .deviceNotFound(let address):
            return "Bluetooth device not found: \(address)"
        case .connectionFailed(let address, let message):
            return "Failed to connect to \(address): \(message)"
        case .disconnectionFailed(let address, let message):
            return "Failed to disconnect from \(address): \(message)"
        case .pairFailed(let address, let message):
            return "Failed to pair with \(address): \(message)"
        case .trustFailed(let address, let message):
            return "Failed to trust \(address): \(message)"
        case .forgetFailed(let address, let message):
            return "Failed to forget device \(address): \(message)"
        }
    }
}
