import Bluetooth
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import WendyAgentGRPC
import WendyShared

/// Client for communicating with a WendyOS agent over Bluetooth L2CAP
///
/// Use the static `withConnection` method to establish a connection and execute commands.
/// This ensures proper cleanup through structured concurrency.
actor BluetoothAgentClient {
    private let connection: PeripheralConnection
    private let channel: any L2CAPChannel
    private let logger: Logger
    private let peripheralId: String

    /// Default timeout for connection establishment
    static let defaultConnectionTimeout: Duration = .seconds(30)

    /// Default timeout for command responses
    static let defaultResponseTimeout: Duration = .seconds(60)

    private init(
        connection: PeripheralConnection,
        channel: any L2CAPChannel,
        logger: Logger,
        peripheralId: String
    ) {
        self.connection = connection
        self.channel = channel
        self.logger = logger
        self.peripheralId = peripheralId
    }

    /// Execute a block with a connected Bluetooth agent client.
    /// Connection and cleanup are handled automatically through structured concurrency.
    ///
    /// - Parameters:
    ///   - peripheral: The Bluetooth peripheral to connect to
    ///   - connectionTimeout: Maximum time to wait for connection (default: 30 seconds)
    ///   - logger: Logger for debug output
    ///   - body: The closure to execute with the connected client
    static func withConnection<R: Sendable>(
        to peripheral: Peripheral,
        connectionTimeout: Duration = defaultConnectionTimeout,
        logger: Logger = Logger(label: "sh.wendy.bluetooth-client"),
        _ body: @escaping @Sendable (BluetoothAgentClient) async throws -> R
    ) async throws -> R {
        let centralManager = CentralManager()
        let peripheralId = peripheral.id.description

        logger.debug("Connecting to peripheral", metadata: ["id": "\(peripheralId)"])

        // Connect to the peripheral with timeout
        let connection: PeripheralConnection
        do {
            connection = try await withThrowingTimeout(of: connectionTimeout) {
                try await centralManager.connect(to: peripheral)
            }
        } catch is TimeoutError {
            throw BluetoothAgentError.connectionTimeout(
                peripheralId: peripheralId,
                timeout: connectionTimeout
            )
        }

        // Wait for connection to be ready with timeout
        let state = await connection.state()
        if state != .connected {
            do {
                try await withThrowingTimeout(of: connectionTimeout) {
                    for await newState in await connection.stateUpdates() {
                        if newState == .connected {
                            return
                        }
                        if case .disconnected = newState {
                            throw BluetoothAgentError.connectionFailed(
                                peripheralId: peripheralId,
                                reason: "Peripheral disconnected during connection"
                            )
                        }
                    }
                }
            } catch is TimeoutError {
                await connection.disconnect()
                throw BluetoothAgentError.connectionTimeout(
                    peripheralId: peripheralId,
                    timeout: connectionTimeout
                )
            }
        }

        logger.debug("Connected, opening L2CAP channel", metadata: ["id": "\(peripheralId)"])

        // Open L2CAP channel on the Wendy PSM
        let psm = L2CAPPSM(rawValue: WendyBluetoothUUIDs.l2capPSM)
        let channel: any L2CAPChannel
        do {
            channel = try await withThrowingTimeout(of: connectionTimeout) {
                try await connection.openL2CAPChannel(psm: psm)
            }
        } catch is TimeoutError {
            await connection.disconnect()
            throw BluetoothAgentError.channelTimeout(peripheralId: peripheralId, psm: psm.rawValue)
        }

        logger.debug(
            "L2CAP channel opened",
            metadata: [
                "id": "\(peripheralId)",
                "psm": "\(psm.rawValue)",
                "mtu": "\(channel.mtu)",
            ]
        )

        let client = BluetoothAgentClient(
            connection: connection,
            channel: channel,
            logger: logger,
            peripheralId: peripheralId
        )

        do {
            let result = try await body(client)
            await channel.close()
            await connection.disconnect()
            return result
        } catch {
            await channel.close()
            await connection.disconnect()
            throw error
        }
    }

    // MARK: - WiFi Commands

    func listWiFiNetworks() async throws -> [Wendy_Agent_Services_V1_WifiNetworkInfo] {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.wifiList = Wendy_Agent_Services_V1_WifiListCommand()

        let response = try await sendCommand(command)
        guard case .wifiList(let wifiList) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return wifiList.networks
    }

    func connectToWiFi(
        ssid: String,
        password: String
    ) async throws -> Wendy_Agent_Services_V1_WifiConnectResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        var wifiConnect = Wendy_Agent_Services_V1_WifiConnectCommand()
        wifiConnect.ssid = ssid
        wifiConnect.password = password
        command.wifiConnect = wifiConnect

        let response = try await sendCommand(command)
        guard case .wifiConnect(let wifiConnect) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return wifiConnect
    }

    func getWiFiStatus() async throws -> Wendy_Agent_Services_V1_WifiStatusResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.wifiStatus = Wendy_Agent_Services_V1_WifiStatusCommand()

        let response = try await sendCommand(command)
        guard case .wifiStatus(let wifiStatus) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return wifiStatus
    }

    func disconnectWiFi() async throws -> Wendy_Agent_Services_V1_WifiDisconnectResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.wifiDisconnect = Wendy_Agent_Services_V1_WifiDisconnectCommand()

        let response = try await sendCommand(command)
        guard case .wifiDisconnect(let wifiDisconnect) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return wifiDisconnect
    }

    // MARK: - Apps Commands

    func listApps() async throws -> [Wendy_Agent_Services_V1_AppInfo] {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.appsList = Wendy_Agent_Services_V1_AppsListCommand()

        let response = try await sendCommand(command)
        guard case .appsList(let appsList) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return appsList.apps
    }

    func stopApp(name: String) async throws -> Wendy_Agent_Services_V1_AppsStopResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        var appsStop = Wendy_Agent_Services_V1_AppsStopCommand()
        appsStop.appName = name
        command.appsStop = appsStop

        let response = try await sendCommand(command)
        guard case .appsStop(let appsStop) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return appsStop
    }

    func removeApp(
        name: String,
        purgeImage: Bool
    ) async throws -> Wendy_Agent_Services_V1_AppsRemoveResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        var appsRemove = Wendy_Agent_Services_V1_AppsRemoveCommand()
        appsRemove.appName = name
        appsRemove.purgeImage = purgeImage
        command.appsRemove = appsRemove

        let response = try await sendCommand(command)
        guard case .appsRemove(let appsRemove) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return appsRemove
    }

    // MARK: - Other Commands

    func getAgentVersion() async throws -> String {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.agentVersion = Wendy_Agent_Services_V1_AgentVersionCommand()

        let response = try await sendCommand(command)
        guard case .agentVersion(let agentVersion) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return agentVersion.version
    }

    func listHardware() async throws -> [Wendy_Agent_Services_V1_HardwareInfo] {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.hardwareList = Wendy_Agent_Services_V1_HardwareListCommand()

        let response = try await sendCommand(command)
        guard case .hardwareList(let hardwareList) = response.response else {
            throw BluetoothAgentError.unexpectedResponse(
                peripheralId: peripheralId,
                command: command.commandName
            )
        }
        return hardwareList.capabilities
    }

    // MARK: - Private Methods

    private func sendCommand(
        _ command: Wendy_Agent_Services_V1_BluetoothCommand,
        timeout: Duration = defaultResponseTimeout
    ) async throws -> Wendy_Agent_Services_V1_BluetoothResponse {
        let commandName = command.commandName

        // Serialize the command
        let commandBytes: [UInt8]
        do {
            commandBytes = try command.serializedBytes()
        } catch {
            throw BluetoothAgentError.serializationFailed(
                peripheralId: peripheralId,
                command: commandName,
                underlying: error
            )
        }

        // Send with length prefix (4-byte big-endian)
        var sendBuffer = ByteBuffer()
        sendBuffer.writeLengthPrefixed(as: UInt32.self, endianness: .big) { buffer in
            buffer.writeData(commandData)
            return buffer.writerIndex
        }

        logger.debug(
            "Sending command",
            metadata: [
                "command": "\(commandName)",
                "length": "\(commandBytes.count)",
                "peripheralId": "\(peripheralId)",
            ]
        )

        try await channel.send(Data(buffer: sendBuffer))

        // Read response with length prefix and timeout
        var responseBuffer: ByteBuffer
        do {
            responseBuffer = try await withThrowingTimeout(of: timeout) {
                try await self.readLengthPrefixedMessage()
            }
        } catch is TimeoutError {
            throw BluetoothAgentError.responseTimeout(
                peripheralId: peripheralId,
                command: commandName,
                timeout: timeout
            )
        }

        // Parse response
        let response: Wendy_Agent_Services_V1_BluetoothResponse
        do {
            response = try Wendy_Agent_Services_V1_BluetoothResponse(
                serializedBytes: responseBuffer.readableBytesView
            )
        } catch {
            throw BluetoothAgentError.deserializationFailed(
                peripheralId: peripheralId,
                command: commandName,
                underlying: error
            )
        }

        // Check for error response
        if case .error(let error) = response.response {
            throw BluetoothAgentError.agentError(
                peripheralId: peripheralId,
                command: commandName,
                message: error.message
            )
        }

        logger.debug(
            "Received response",
            metadata: [
                "command": "\(commandName)",
                "peripheralId": "\(peripheralId)",
            ]
        )

        return response
    }

    private func readLengthPrefixedMessage() async throws -> ByteBuffer {
        var buffer = ByteBuffer()

        for try await data in channel.incoming() {
            buffer.writeData(data)

            // Try to read a length-prefixed message
            if let message = buffer.readLengthPrefixed(as: UInt32.self, endianness: .big) {
                return message
            }
        }

        throw BluetoothAgentError.connectionClosed(peripheralId: peripheralId)
    }
}

enum BluetoothAgentError: Error, CustomStringConvertible {
    case connectionTimeout(peripheralId: String, timeout: Duration)
    case connectionFailed(peripheralId: String, reason: String)
    case channelTimeout(peripheralId: String, psm: UInt16)
    case connectionClosed(peripheralId: String)
    case responseTimeout(peripheralId: String, command: String, timeout: Duration)
    case unexpectedResponse(peripheralId: String, command: String)
    case serializationFailed(peripheralId: String, command: String, underlying: Error)
    case deserializationFailed(peripheralId: String, command: String, underlying: Error)
    case agentError(peripheralId: String, command: String, message: String)

    var description: String {
        switch self {
        case .connectionTimeout(let peripheralId, let timeout):
            return "Connection to \(peripheralId) timed out after \(timeout)"
        case .connectionFailed(let peripheralId, let reason):
            return "Connection to \(peripheralId) failed: \(reason)"
        case .channelTimeout(let peripheralId, let psm):
            return "L2CAP channel (PSM \(psm)) to \(peripheralId) timed out"
        case .connectionClosed(let peripheralId):
            return "Connection to \(peripheralId) closed unexpectedly"
        case .responseTimeout(let peripheralId, let command, let timeout):
            return "Response for '\(command)' from \(peripheralId) timed out after \(timeout)"
        case .unexpectedResponse(let peripheralId, let command):
            return "Unexpected response for '\(command)' from \(peripheralId)"
        case .serializationFailed(let peripheralId, let command, let underlying):
            return "Failed to serialize '\(command)' for \(peripheralId): \(underlying)"
        case .deserializationFailed(let peripheralId, let command, let underlying):
            return
                "Failed to deserialize response for '\(command)' from \(peripheralId): \(underlying)"
        case .agentError(let peripheralId, let command, let message):
            return "Agent \(peripheralId) returned error for '\(command)': \(message)"
        }
    }
}

// MARK: - Timeout Helper

private struct TimeoutError: Error {}

private func withThrowingTimeout<T: Sendable>(
    of duration: Duration,
    _ operation: @escaping @Sendable () async throws -> T
) async throws -> T {
    try await withThrowingTaskGroup(of: T.self) { group in
        group.addTask {
            try await operation()
        }
        group.addTask {
            try await Task.sleep(for: duration)
            throw TimeoutError()
        }

        let result = try await group.next()!
        group.cancelAll()
        return result
    }
}

// MARK: - Command Name Helper

extension Wendy_Agent_Services_V1_BluetoothCommand {
    var commandName: String {
        switch self.command {
        case .wifiList: return "wifiList"
        case .wifiConnect: return "wifiConnect"
        case .wifiStatus: return "wifiStatus"
        case .wifiDisconnect: return "wifiDisconnect"
        case .appsList: return "appsList"
        case .appsStop: return "appsStop"
        case .appsRemove: return "appsRemove"
        case .agentVersion: return "agentVersion"
        case .hardwareList: return "hardwareList"
        case .none: return "unknown"
        }
    }
}
