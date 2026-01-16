import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOCore
import NIOFoundationCompat
import NIOSSL
import Noora
import WendyAgentGRPC
import WendyCloudGRPC
import WendyShared

#if canImport(Bluetooth)
    import Bluetooth
#endif

typealias GRPCTransport = HTTP2ClientTransport.Posix

func withGRPCClient<R: Sendable>(
    _ endpoint: AgentConnectionOptions.Endpoint,
    security: GRPCTransport.TransportSecurity,
    _ body: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R
) async throws -> R {
    let transport = try GRPCTransport(
        target: .dns(
            host: endpoint.host,
            port: endpoint.port
        ),
        transportSecurity: security
    )

    return try await withGRPCClient(transport: transport) { client in
        try await body(client)
    }
}

func withCloudGRPCClient<R: Sendable>(
    auth: Config.Auth,
    _ body: @escaping @Sendable (CloudGRPCClient) async throws -> R
) async throws -> R {
    let endpoint = AgentConnectionOptions.Endpoint(
        host: auth.cloudGRPC,
        port: 50052
    )
    guard let cert = auth.certificates.first else {
        throw RPCError(code: .aborted, message: "No certificate found")
    }

    return try await withGRPCClient(
        endpoint,
        security: .mTLS(
            certificateChain: cert.certificateChainPEM.map { cert in
                return TLSConfig.CertificateSource.bytes(Array(cert.utf8), format: .pem)
            },
            privateKey: .bytes(Array(cert.privateKeyPEM.utf8), format: .pem)
        ) { tls in
            #if DEBUG
                tls.serverCertificateVerification = .noVerification
            #endif
        }
    ) { client in
        let client = CloudGRPCClient(
            grpc: client,
            cloudHost: auth.cloudGRPC,
            metadata: Metadata()
        )
        return try await body(client)
    }
}

func withCloudGRPCClient<R: Sendable>(
    title: TerminalText,
    _ body: @escaping @Sendable (CloudGRPCClient) async throws -> R
) async throws -> R {
    return try await withAuth(title: title) { auth -> R in
        return try await withCloudGRPCClient(auth: auth) { client in
            return try await body(client)
        }
    }
}

private enum ProvisioningResult<R: Sendable>: Sendable {
    case notProvisioned(R)
    case retryWithProvisioned(assetId: Int32, organizationId: Int32)
}

func withAgentGRPCClient<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: TerminalText,
    _ body: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R
) async throws -> R {
    let endpoint = try await connectionOptions.read(title: title)
    do {
        return try await withAgentGRPCClient(endpoint, title: title) { client in
            return try await body(client)
        }
    } catch  where endpoint.defaultDevice {
        let endpoint = try await connectionOptions.read(
            title: title,
            readDefault: false
        )
        return try await withAgentGRPCClient(endpoint, title: title) { client in
            return try await body(client)
        }
    }
}

func withAgentGRPCClient<R: Sendable>(
    _ endpoint: AgentConnectionOptions.Endpoint,
    title: TerminalText,
    _ body: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R
) async throws -> R {
    return try await _withAgentGRPCClient(endpoint, title: title) { client, _ in
        return try await body(client)
    }
}

func withAgentGRPCClientAndEndpoint<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: TerminalText,
    _ body:
        @escaping @Sendable (GRPCClient<GRPCTransport>, AgentConnectionOptions.Endpoint)
        async throws -> R
) async throws -> R {
    let endpoint = try await connectionOptions.read(title: title)
    do {
        return try await _withAgentGRPCClient(endpoint, title: title) { client, endpoint in
            return try await body(client, endpoint)
        }
    } catch  where endpoint.defaultDevice {
        let endpoint = try await connectionOptions.read(
            title: title,
            readDefault: false
        )
        return try await _withAgentGRPCClient(endpoint, title: title) { client, endpoint in
            return try await body(client, endpoint)
        }
    }
}

func _withAgentGRPCClient<R: Sendable>(
    _ endpoint: AgentConnectionOptions.Endpoint,
    title: TerminalText,
    _ body:
        @escaping @Sendable (GRPCClient<GRPCTransport>, AgentConnectionOptions.Endpoint)
        async throws -> R
) async throws -> R {
    let logger = Logger(label: "sh.wendy.agent-grpc-client")
    do {
        let result = try await withGRPCClient(endpoint, security: .plaintext) {
            client -> ProvisioningResult<R> in
            let provisioningAPI = Wendy_Agent_Services_V1_WendyProvisioningService.Client(
                wrapping: client
            )
            let response = try await provisioningAPI.isProvisioned(.init())
            switch response.response {
            case .notProvisioned:
                return .notProvisioned(try await body(client, endpoint))
            case .provisioned, .none:
                return .retryWithProvisioned(
                    assetId: response.provisioned.assetID,
                    organizationId: response.provisioned.organizationID
                )
            }
        }

        switch result {
        case .notProvisioned(let result):
            return result
        case .retryWithProvisioned(let assetId, let organizationId):
            return try await withCertificates(
                title: title,
                forOrganizationId: organizationId
            ) { certificate in
                var endpoint = endpoint
                endpoint.port += 1
                return try await withGRPCClient(
                    endpoint,
                    security: .mTLS(
                        certificateChain: certificate.certificateChainPEM.map { cert in
                            return TLSConfig.CertificateSource.bytes(Array(cert.utf8), format: .pem)
                        },
                        privateKey: .bytes(
                            Array(certificate.privateKeyPEM.utf8),
                            format: .pem
                        )
                    ) { tls in
                        tls.serverCertificateVerification = .noHostnameVerification
                        tls.customVerificationCallback = { certs, promise in
                            guard
                                let cert = certs.first,
                                cert._subjectAlternativeNames().contains(where: { name in
                                    name.contents.contains("urn:wendy:org:\(organizationId)".utf8)
                                        && name.contents.contains(
                                            "urn:wendy:org:\(organizationId):asset:\(assetId)".utf8
                                        )
                                })
                            else {
                                promise.succeed(.failed)
                                return
                            }

                            promise.succeed(
                                .certificateVerified(
                                    .init(
                                        NIOSSL.ValidatedCertificateChain(certs)
                                    )
                                )
                            )
                        }
                    }
                ) { [endpoint] client in
                    return try await body(client, endpoint)
                }
            }
        }
    } catch let error as RPCError where error.code == .unavailable {
        logger.debug(
            "Could not connect to host",
            metadata: [
                "host": "\(endpoint.host)",
                "port": "\(endpoint.port)",
            ]
        )
        throw error
    } catch let error as ChannelError {
        // This is the error we expect, but gRPC kicks off its own error
        logger.debug(
            "Could not connect to host",
            metadata: [
                "host": "\(endpoint.host)",
                "port": "\(endpoint.port)",
            ]
        )
        throw error
    }
}

// MARK: - Bluetooth Connection Helpers

#if canImport(Bluetooth)

    enum BluetoothConnectionError: Error, LocalizedError {
        case deviceNotFound
        case connectionFailed
        case connectionTimeout
        case noResponse
        case bluetoothUnavailable

        var errorDescription: String? {
            switch self {
            case .deviceNotFound:
                return "Device not found"
            case .connectionFailed:
                return "Failed to establish Bluetooth connection"
            case .connectionTimeout:
                return "Bluetooth connection timed out"
            case .noResponse:
                return "No response received from device"
            case .bluetoothUnavailable:
                return "Bluetooth is not available"
            }
        }
    }

    /// Connect to a WendyOS device over Bluetooth using a peripheral and execute a command
    /// - Parameters:
    ///   - central: The CentralManager that discovered the peripheral (required on CoreBluetooth)
    ///   - peripheral: The peripheral to connect to
    ///   - l2capPSM: Optional L2CAP PSM to use (defaults to WendyBluetoothUUIDs.l2capPSM)
    ///   - operation: The operation to perform on the L2CAP channel
    func withBluetoothConnection<T>(
        central: CentralManager,
        peripheral: Peripheral,
        l2capPSM: UInt16? = nil,
        operation: (L2CAPChannel) async throws -> T
    ) async throws -> T {
        let logger = Logger(label: "sh.wendy.cli.bluetooth")

        logger.debug("Connecting to device", metadata: ["peripheral": "\(peripheral.id.rawValue)"])

        // Connect to the device using the same CentralManager that discovered it
        let connection = try await central.connect(to: peripheral)

        // Use provided PSM or fall back to default
        let psm = L2CAPPSM(rawValue: l2capPSM ?? WendyBluetoothUUIDs.l2capPSM)

        // Open L2CAP channel
        let channel = try await connection.openL2CAPChannel(psm: psm)

        logger.debug("L2CAP channel opened")

        defer {
            Task {
                await channel.close()
                await connection.disconnect()
            }
        }

        return try await operation(channel)
    }

    /// Result of establishing a Bluetooth connection
    private struct BluetoothConnectionResult: Sendable {
        let connection: PeripheralConnection
        let channel: L2CAPChannel
    }

    /// Connect to a WendyOS device directly by identifier and execute a command
    func withBluetoothConnection<T: Sendable>(
        deviceIdentifier: String,
        timeout: Int = 30,
        operation: @Sendable (L2CAPChannel) async throws -> T
    ) async throws -> T {
        let logger = Logger(label: "sh.wendy.cli.bluetooth")
        let central = CentralManager()

        // Wait for Bluetooth to be ready
        let currentState = await central.state()
        switch currentState {
        case .poweredOn:
            break
        case .poweredOff, .unauthorized, .unsupported:
            throw BluetoothConnectionError.bluetoothUnavailable
        default:
            var ready = false
            for await state in await central.stateUpdates() {
                switch state {
                case .poweredOn:
                    ready = true
                case .poweredOff, .unauthorized, .unsupported:
                    throw BluetoothConnectionError.bluetoothUnavailable
                default:
                    continue
                }
                break
            }
            guard ready else {
                throw BluetoothConnectionError.bluetoothUnavailable
            }
        }

        logger.debug(
            "Connecting directly to device",
            metadata: ["identifier": "\(deviceIdentifier)"]
        )

        // Create a Peripheral from the identifier
        // Note: The identifier should include the uuid: or addr: prefix as expected by the backend
        let peripheral = Peripheral(id: BluetoothDeviceID(deviceIdentifier))

        // Connect to the device with a progress spinner and timeout
        let result: BluetoothConnectionResult = try await Noora().progressStep(
            message: "Connecting to Bluetooth device",
            successMessage: "Connected to Bluetooth device",
            errorMessage: "Failed to connect to Bluetooth device",
            showSpinner: true
        ) { _ in
            try await withThrowingTaskGroup(of: BluetoothConnectionResult.self) { group in
                group.addTask {
                    let connection = try await central.connect(to: peripheral)
                    let psm = L2CAPPSM(rawValue: WendyBluetoothUUIDs.l2capPSM)
                    let channel = try await connection.openL2CAPChannel(psm: psm)
                    return BluetoothConnectionResult(connection: connection, channel: channel)
                }

                group.addTask {
                    try await Task.sleep(for: .seconds(timeout))
                    throw BluetoothConnectionError.connectionTimeout
                }

                let result = try await group.next()!
                group.cancelAll()
                return result
            }
        }

        defer {
            Task {
                await result.channel.close()
                await result.connection.disconnect()
            }
        }

        return try await operation(result.channel)
    }

    /// Execute a Bluetooth command and return the response
    func executeBluetoothCommand(
        _ command: BluetoothAgentCommand,
        deviceIdentifier: String,
        timeout: Int = 30
    ) async throws -> BluetoothResponse {
        let logger = Logger(label: "sh.wendy.cli.bluetooth")
        return try await withBluetoothConnection(deviceIdentifier: deviceIdentifier) { channel in
            // Send length-prefixed command
            let commandData = try command.toData()
            var buffer = ByteBuffer()
            buffer.writeInteger(UInt32(commandData.count), endianness: .big)
            buffer.writeData(commandData)

            logger.debug("Sending command", metadata: ["size": "\(commandData.count)"])
            try await channel.send(Data(buffer.readableBytesView))
            logger.debug("Command sent, waiting for response...")

            // Wait for length-prefixed response with timeout
            return try await withThrowingTaskGroup(of: BluetoothResponse.self) { group in
                group.addTask {
                    var buffer = ByteBuffer()
                    var expectedLength: Int?

                    for try await data in channel.incoming() {
                        buffer.writeData(data)

                        // Read length prefix if we don't have it yet
                        if expectedLength == nil && buffer.readableBytes >= 4 {
                            expectedLength = Int(buffer.readInteger(endianness: .big, as: UInt32.self)!)
                            logger.debug("Response length", metadata: ["expected": "\(expectedLength!)"])
                        }

                        // Check if we have complete response
                        if let length = expectedLength, buffer.readableBytes >= length {
                            let responseData = buffer.readData(length: length)!
                            logger.debug("Received complete response", metadata: ["size": "\(responseData.count)"])
                            return try BluetoothResponse.from(data: responseData)
                        }
                    }
                    throw BluetoothConnectionError.noResponse
                }

                group.addTask {
                    try await Task.sleep(for: .seconds(timeout))
                    throw BluetoothConnectionError.noResponse
                }

                let result = try await group.next()!
                group.cancelAll()
                return result
            }
        }
    }

#endif

// MARK: - Unified Agent Connection

/// Execute an operation using either gRPC or Bluetooth based on connection options
func withAgentConnection<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: TerminalText,
    grpcOperation: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R,
    bluetoothOperation: @escaping @Sendable (String) async throws -> R
) async throws -> R {
    let connectionType = try await connectionOptions.readConnectionType(title: title)

    switch connectionType {
    case .grpc(let endpoint):
        return try await withAgentGRPCClient(endpoint, title: title) { client in
            try await grpcOperation(client)
        }
    case .bluetooth(let deviceIdentifier):
        #if canImport(Bluetooth)
            let logger = Logger(label: "sh.wendy.cli.bluetooth")
            logger.debug(
                "Using Bluetooth connection",
                metadata: ["identifier": "\(deviceIdentifier)"]
            )
            return try await bluetoothOperation(deviceIdentifier)
        #else
            throw BluetoothNotAvailableError()
        #endif
    }
}

struct BluetoothNotAvailableError: Error, LocalizedError {
    var errorDescription: String? {
        "Bluetooth is not available on this platform"
    }
}
