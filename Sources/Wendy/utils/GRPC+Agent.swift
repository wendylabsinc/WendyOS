import CLIOutput
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOCore
import NIOSSL
import Synchronization
import WendyAgentGRPC
import WendyCloudGRPC

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
    title: String,
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
    title: String,
    _ body: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R
) async throws -> R {
    return try await withAgentGRPCClientAndEndpoint(connectionOptions, title: title) { client, _ in
        return try await body(client)
    }
}

func withAgentGRPCClient<R: Sendable>(
    _ endpoint: AgentConnectionOptions.Endpoint,
    title: String,
    _ body: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R
) async throws -> R {
    return try await _withAgentGRPCClient(endpoint, title: title) { client, _ in
        return try await body(client)
    }
}

func withAgentGRPCClientAndEndpoint<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: String,
    _ body:
        @escaping @Sendable (GRPCClient<GRPCTransport>, AgentConnectionOptions.Endpoint)
        async throws -> R
) async throws -> R {
    func fallback() async throws -> R {
        switch try await connectionOptions.read(
            title: title,
            readDefault: false,
            includeBluetooth: false
        ) {
        case .lan(let host, let port, let defaultDevice):
            let endpoint = AgentConnectionOptions.Endpoint(
                host: host,
                port: port,
                defaultDevice: defaultDevice
            )
            return try await withAgentGRPCClient(endpoint, title: title) { client in
                return try await body(client, endpoint)
            }
        case .bluetooth:
            throw CancellationError()
        }
    }

    switch try await connectionOptions.read(title: title) {
    case .lan(let host, let port, let defaultDevice):
        let connectionSucceeded = Mutex(false)
        do {
            let endpoint = AgentConnectionOptions.Endpoint(
                host: host,
                port: port,
                defaultDevice: defaultDevice
            )
            return try await withAgentGRPCClient(endpoint, title: title) { client in
                connectionSucceeded.withLock { $0 = true }
                return try await body(client, endpoint)
            }
        } catch {
            // Only retry with device selection if we never successfully connected
            guard defaultDevice && !connectionSucceeded.withLock({ $0 }) else {
                throw error
            }
            return try await fallback()
        }
    case .bluetooth:
        return try await fallback()
    }
}

func _withAgentGRPCClient<R: Sendable>(
    _ endpoint: AgentConnectionOptions.Endpoint,
    title: String,
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

/// Execute a gRPC operation with automatic handling of unimplemented API errors.
/// If the device returns an unimplemented error, prompts the user to update their device.
func withAgentGRPCClientHandlingUpdates<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: String,
    _ body: @escaping @Sendable (GRPCClient<GRPCTransport>) async throws -> R
) async throws -> R {
    do {
        return try await withAgentGRPCClientAndEndpoint(connectionOptions, title: title) {
            client,
            _ in
            try await body(client)
        }
    } catch let error as RPCError where error.code == .unimplemented {
        // Get the endpoint for potential update
        let selectedDevice = try await connectionOptions.read(
            title: title,
            includeBluetooth: false
        )

        guard case .lan(let host, let port, let defaultDevice) = selectedDevice else {
            throw error
        }

        let endpoint = AgentConnectionOptions.Endpoint(
            host: host,
            port: port,
            defaultDevice: defaultDevice
        )

        // Prompt user to update - if they decline or update fails, re-throw original error
        let didUpdate = await promptDeviceUpdateIfUnimplemented(error: error, endpoint: endpoint)
        if !didUpdate {
            throw error
        }

        // User updated successfully - they need to retry the command
        throw CancellationError()
    }
}

/// Wait for the gRPC socket to come back up after a device restart
func waitForDeviceRestart(endpoint: AgentConnectionOptions.Endpoint) async throws {
    try await cliOutput.withProgress(
        message: "Waiting for device to restart...",
        successMessage: "Device restarted successfully",
        errorMessage: "Device failed to restart"
    ) {
        let maxRetries = 90  // Wait up to 90 seconds
        let retryDelay: UInt64 = 1_000_000_000  // 1 second in nanoseconds

        // Initial wait for the device to go down
        try await Task.sleep(nanoseconds: 3 * retryDelay)

        // Now try to reconnect
        for attempt in 1...maxRetries {
            _ = attempt  // Used for retry counting
            do {
                // Try to connect and verify the agent is responsive
                try await withAgentGRPCClient(endpoint, title: "") { client in
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(
                        wrapping: client
                    )
                    _ = try await agent.getAgentVersion(request: .init(message: .init()))
                }
                // Connection succeeded, device is back up
                return
            } catch {
                // Connection failed, wait and retry
                try await Task.sleep(nanoseconds: retryDelay)
                continue
            }
        }

        throw RPCError(code: .unavailable, message: "Device did not come back up after update")
    }
}
