import Foundation
import Logging
import Subprocess

/// Manages Bluetooth operations using BlueZ (bluetoothctl)
struct BluetoothManager: Sendable {
    let logger: Logger
    private static let defaultScanTimeoutSeconds: UInt32 = 20

    struct BluetoothDeviceInfo: Sendable {
        let name: String
        let address: String
        let rssi: Int?
        let paired: Bool
        let connected: Bool
        let trusted: Bool
        let deviceType: String
        let icon: String?
    }

    /// Lists Bluetooth devices using bluetoothctl
    func listDevices(pairedOnly: Bool) async throws -> [BluetoothDeviceInfo] {
        // First, get the list of device addresses
        let devicesOutput = try await runBluetoothctl(["devices"])
        let deviceAddresses = parseDeviceAddresses(from: devicesOutput)

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
        return parseDeviceInfo(from: infoOutput, address: address)
    }

    /// Starts Bluetooth discovery scan
    func startScan(timeoutSeconds: UInt32) async throws {
        // Power on the adapter first
        try await powerOn()

        // bluetoothctl scan needs a timeout in non-interactive mode to actually discover devices.
        let effectiveTimeoutSeconds =
            timeoutSeconds == 0 ? Self.defaultScanTimeoutSeconds : timeoutSeconds

        _ = Task.detached { [logger, self] in
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
            } catch {
                logger.warning(
                    "bluetoothctl scan failed",
                    metadata: ["error": "\(error)"]
                )
            }
        }

        // If a timeout is specified, schedule stopping the scan
        if timeoutSeconds > 0 {
            Task { [logger] in
                do {
                    try await Task.sleep(for: .seconds(Int(timeoutSeconds)))
                    try await stopScan()
                } catch {
                    logger.warning(
                        "Failed to stop bluetooth scan after timeout",
                        metadata: ["error": "\(error)"]
                    )
                }
            }
        }
    }

    /// Stops Bluetooth discovery scan
    func stopScan() async throws {
        _ = try await runBluetoothctl(["scan", "off"])
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
    private func parseDeviceAddresses(from output: String) -> [String] {
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
    private func parseDeviceInfo(from output: String, address: String) -> BluetoothDeviceInfo? {
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
