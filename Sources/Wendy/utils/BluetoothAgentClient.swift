import Bluetooth
import Foundation
import Logging
import NIOCore
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

    private init(connection: PeripheralConnection, channel: any L2CAPChannel, logger: Logger) {
        self.connection = connection
        self.channel = channel
        self.logger = logger
    }

    /// Execute a block with a connected Bluetooth agent client.
    /// Connection and cleanup are handled automatically through structured concurrency.
    static func withConnection<R: Sendable>(
        to peripheral: Peripheral,
        logger: Logger = Logger(label: "sh.wendy.bluetooth-client"),
        _ body: @escaping @Sendable (BluetoothAgentClient) async throws -> R
    ) async throws -> R {
        let centralManager = CentralManager()
        try await centralManager.waitUntilReady()

        logger.debug("Connecting to peripheral", metadata: ["id": "\(peripheral.id)"])

        // Connect to the peripheral
        let connection = try await centralManager.connect(to: peripheral)

        // Wait for connection to be ready
        let state = await connection.state()
        findConnection: if state != .connected {
            for await newState in await connection.stateUpdates() {
                if newState == .connected {
                    break findConnection
                }
                if case .disconnected = newState {
                    throw BluetoothAgentError.connectionFailed("Peripheral disconnected")
                }
            }
        }

        logger.debug("Connected, opening L2CAP channel")

        // Open L2CAP channel on the Wendy PSM
        let psm = L2CAPPSM(rawValue: WendyBluetoothUUIDs.l2capPSM)
        let channel = try await connection.openL2CAPChannel(psm: psm)

        logger.debug(
            "L2CAP channel opened",
            metadata: ["psm": "\(psm.rawValue)", "mtu": "\(channel.mtu)"]
        )

        let client = BluetoothAgentClient(connection: connection, channel: channel, logger: logger)

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
            throw BluetoothAgentError.unexpectedResponse
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
            throw BluetoothAgentError.unexpectedResponse
        }
        return wifiConnect
    }

    func getWiFiStatus() async throws -> Wendy_Agent_Services_V1_WifiStatusResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.wifiStatus = Wendy_Agent_Services_V1_WifiStatusCommand()

        let response = try await sendCommand(command)
        guard case .wifiStatus(let wifiStatus) = response.response else {
            throw BluetoothAgentError.unexpectedResponse
        }
        return wifiStatus
    }

    func disconnectWiFi() async throws -> Wendy_Agent_Services_V1_WifiDisconnectResponse {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.wifiDisconnect = Wendy_Agent_Services_V1_WifiDisconnectCommand()

        let response = try await sendCommand(command)
        guard case .wifiDisconnect(let wifiDisconnect) = response.response else {
            throw BluetoothAgentError.unexpectedResponse
        }
        return wifiDisconnect
    }

    // MARK: - Apps Commands

    func listApps() async throws -> [Wendy_Agent_Services_V1_AppInfo] {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.appsList = Wendy_Agent_Services_V1_AppsListCommand()

        let response = try await sendCommand(command)
        guard case .appsList(let appsList) = response.response else {
            throw BluetoothAgentError.unexpectedResponse
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
            throw BluetoothAgentError.unexpectedResponse
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
            throw BluetoothAgentError.unexpectedResponse
        }
        return appsRemove
    }

    // MARK: - Other Commands

    func getAgentVersion() async throws -> String {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.agentVersion = Wendy_Agent_Services_V1_AgentVersionCommand()

        let response = try await sendCommand(command)
        guard case .agentVersion(let agentVersion) = response.response else {
            throw BluetoothAgentError.unexpectedResponse
        }
        return agentVersion.version
    }

    func listHardware() async throws -> [Wendy_Agent_Services_V1_HardwareInfo] {
        var command = Wendy_Agent_Services_V1_BluetoothCommand()
        command.hardwareList = Wendy_Agent_Services_V1_HardwareListCommand()

        let response = try await sendCommand(command)
        guard case .hardwareList(let hardwareList) = response.response else {
            throw BluetoothAgentError.unexpectedResponse
        }
        return hardwareList.capabilities
    }

    // MARK: - Private Methods

    private func sendCommand(
        _ command: Wendy_Agent_Services_V1_BluetoothCommand
    ) async throws -> Wendy_Agent_Services_V1_BluetoothResponse {
        // Serialize the command
        let commandData = try command.serializedData()

        // Send with length prefix (4-byte big-endian)
        var sendBuffer = ByteBuffer()
        try sendBuffer.writeLengthPrefixed(endianness: .big, as: UInt16.self) { buffer in
            buffer.writeData(commandData)
        }

        logger.debug("Sending command", metadata: ["length": "\(commandData.count)"])

        try await channel.send(Data(buffer: sendBuffer))

        // Read response with length prefix
        let responseData = try await readLengthPrefixedMessage()

        // Parse response
        let response = try Wendy_Agent_Services_V1_BluetoothResponse(serializedBytes: responseData)

        // Check for error response
        if case .error(let error) = response.response {
            throw BluetoothAgentError.agentError(error.message)
        }

        return response
    }

    private func readLengthPrefixedMessage() async throws -> Data {
        var buffer = ByteBuffer()

        nextPacket: for try await data in channel.incoming() {
            buffer.writeData(data)

            // Read length prefix if we haven't yet
            guard let slice = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
            else {
                continue nextPacket
            }

            return Data(buffer: slice)
        }

        throw BluetoothAgentError.connectionClosed
    }
}

enum BluetoothAgentError: Error, CustomStringConvertible {
    case connectionFailed(String)
    case connectionClosed
    case unexpectedResponse
    case agentError(String)

    var description: String {
        switch self {
        case .connectionFailed(let message):
            return "Connection failed: \(message)"
        case .connectionClosed:
            return "Connection closed unexpectedly"
        case .unexpectedResponse:
            return "Received unexpected response from agent"
        case .agentError(let message):
            return "Agent error: \(message)"
        }
    }
}
