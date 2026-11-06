import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOCore
import NIOPosix
import WendyCloudGRPC

/// Dials the Wendy cloud tunnel broker and, while connected, makes this
/// provisioned device remotely reachable: it registers presence (so the device
/// shows online) and, on each broker-initiated `DialRequest`, relays a byte
/// stream between the cloud and the device's own local mTLS port. End-to-end
/// mTLS terminates at the agent's real mTLS server; the broker and this client's
/// outer TLS only carry ciphertext.
///
/// Mirrors the Go agent's `TunnelBrokerClient`
/// (`go/internal/agent/services/tunnel_broker_client.go`). The work is behind an
/// injectable `runSession` seam (like `CloudCertificateClient`) so tests drive
/// dial handling without a network; `.live` performs the real dial + relay.
///
/// The pure helpers (`brokerURL`, `identityHeader`, `isLoopback`, `remapPort`,
/// `backoff`) and the two directional relay pumps are decoupled from the gRPC
/// and NIO types (they work in terms of `[UInt8]` and the small `TunnelOutbound`
/// / `TunnelInbound` types) so they are unit-tested with only `WendyAgentCore`
/// in scope. The `.live` dial/relay glue (grpc-swift bidi streaming + a raw NIO
/// socket + broker TLS) cannot be compiled or E2E-tested on the current dev box
/// (the swift-crypto vs macOS-27 SDK build blocker fails the dependency before
/// agent code compiles); its behavior is CI/hardware gated, consistent with the
/// rest of PR #1402.
struct TunnelBrokerClient: Sendable {
    /// The well-known mTLS port the CLI always asks the broker to reach. When the
    /// agent's actual mTLS port differs, a request for this port is remapped.
    static let defaultMTLSPort = 50052

    /// Heartbeat cadence on the presence stream.
    static let heartbeatInterval: Duration = .seconds(30)

    /// The longest wait between reconnect attempts.
    static let maxBackoff: Duration = .seconds(90)

    struct Config: Sendable {
        var brokerURL: String
        var orgID: Int32
        var assetID: Int32
        /// The device's own leaf certificate (PEM), presented to the broker for
        /// mTLS. Empty on an unprovisioned device.
        var certificatePEM: String
        /// How the private key for `certificatePEM` is stored: software PEM or
        /// Secure Enclave. `.softwarePEM("")` on an unprovisioned device.
        var keyBacking: ProvisioningStore.KeyBacking
        /// The loaded Secure Enclave signer, required when `keyBacking` is
        /// `.secureEnclave`.
        var seKey: SEPrivateKey?
        var chainPEM: String
        var mtlsPort: Int
    }

    /// A message the agent writes to the broker on an `AgentTunnel` stream,
    /// abstracted so the relay pump is independent of the generated proto type.
    enum TunnelOutbound: Sendable, Equatable {
        /// The join message: echoes the session id with an empty payload so the
        /// broker can pair this stream with the waiting CLI stream.
        case join(sessionID: String)
        /// A payload chunk read from the local mTLS connection.
        case data([UInt8])
        /// The local connection reached EOF; signal the far side.
        case halfClose
    }

    /// A message the agent receives from the broker on an `AgentTunnel` stream,
    /// abstracted so the relay pump is independent of the generated proto type.
    struct TunnelInbound: Sendable, Equatable {
        var payload: [UInt8]
        var halfClose: Bool
    }

    /// Runs ONE presence session end-to-end — dial the broker, register presence,
    /// heartbeat, and serve/relay every `DialRequest` — returning when the session
    /// ends cleanly, or throwing when it fails. `.live` performs the real gRPC
    /// dial + relay; tests inject a fake.
    var runSession: @Sendable (_ config: Config) async throws -> Void

    /// Backoff/reconnect loop around `runSession`; the entry point `WendyAgent`
    /// starts. Cancellation-aware: returns promptly when the enclosing task is
    /// cancelled (e.g. the device unprovisions or the agent stops).
    func runForever(config: Config, logger: Logger) async {
        var attempt = 0
        while !Task.isCancelled {
            let started = ContinuousClock.now
            do {
                try await self.runSession(config)
                // Clean end (broker closed the stream) — reconnect promptly.
                attempt = 0
            } catch {
                if Task.isCancelled { return }
                // A session that outlived the longest backoff was healthy.
                if ContinuousClock.now - started >= Self.maxBackoff { attempt = 0 }
                let delay = Self.backoff(attempt: attempt)
                attempt += 1
                logger.warning(
                    "broker connection failed, reconnecting",
                    metadata: ["error": "\(error)", "backoff": "\(delay)"]
                )
                do {
                    try await Task.sleep(for: delay)
                } catch {
                    return  // cancelled while backing off
                }
            }
        }
    }

    // MARK: - Pure helpers (unit-tested)

    /// The broker URL: `WENDY_BROKER_URL` override if set, else derived from the
    /// provisioning `cloudHost` (`:443` verbatim, otherwise `<host>:50052`).
    /// Mirrors the Go agent's `brokerURLForCloudHost`.
    static func brokerURL(cloudHost: String, override: String?) -> String {
        if let override, !override.isEmpty { return override }
        if let (host, port) = Self.splitHostPort(cloudHost) {
            return port == 443 ? cloudHost : "\(host):50052"
        }
        return "\(cloudHost):50052"
    }

    /// Splits `host:port`, returning `nil` when there is no trailing numeric port.
    static func splitHostPort(_ s: String) -> (host: String, port: Int)? {
        guard let colon = s.lastIndex(of: ":"),
            let port = Int(s[s.index(after: colon)...]),
            port > 0
        else {
            return nil
        }
        return (String(s[..<colon]), port)
    }

    /// The application-layer identity the broker authenticates on: the device's
    /// own org + asset, as a wendy URN. Mirrors the Go agent's `certHeader`.
    static func identityHeader(orgID: Int32, assetID: Int32) -> String {
        "URI=urn:wendy:org:\(orgID):asset:\(assetID)"
    }

    /// SSRF guard: a `DialRequest` is served only for a loopback target.
    static func isLoopback(_ host: String) -> Bool {
        if host == "localhost" || host == "::1" { return true }
        let parts = host.split(separator: ".", omittingEmptySubsequences: false)
        return parts.count == 4 && parts[0] == "127" && parts.allSatisfy { UInt8($0) != nil }
    }

    /// Remaps the well-known mTLS port to the agent's actual mTLS port when they
    /// differ; leaves any other requested port untouched.
    static func remapPort(requested: Int, mtlsPort: Int) -> Int {
        if mtlsPort != 0, requested == Self.defaultMTLSPort, mtlsPort != Self.defaultMTLSPort {
            return mtlsPort
        }
        return requested
    }

    /// Exponential backoff: `min(1s * 2^attempt, 90s)`.
    static func backoff(attempt: Int) -> Duration {
        let seconds = min(pow(2.0, Double(max(0, attempt))), Self.maxBackoff / .seconds(1))
        return .seconds(seconds)
    }

    // MARK: - Relay pumps (unit-tested with fakes)

    /// Backend → cloud: reads the local mTLS connection and forwards it to the
    /// broker. Emits the join event (echoing the session id) first so the broker
    /// can pair the stream, then one `data` event per chunk, then a final
    /// `halfClose`. Errors end the session; teardown is the caller's concern.
    ///
    /// Takes the raw inbound sequence plus a `@Sendable` byte extractor rather
    /// than a pre-mapped `[UInt8]` sequence: the raw NIO inbound stream is itself
    /// `Sendable`, whereas a `.map`-wrapped sequence's `Sendable` conformance is
    /// not guaranteed, and this pump is captured in a `@Sendable` producer closure.
    static func pumpLocalToGRPC<Inbound: AsyncSequence & Sendable>(
        sessionID: String,
        inbound: Inbound,
        bytes: @Sendable (Inbound.Element) -> [UInt8],
        send: @Sendable (TunnelOutbound) async throws -> Void
    ) async {
        do {
            try await send(.join(sessionID: sessionID))
            for try await element in inbound {
                let chunk = bytes(element)
                if !chunk.isEmpty {
                    try await send(.data(chunk))
                }
            }
            try await send(.halfClose)
        } catch {
            // Session ended (cancelled, stream/socket closed); nothing to add.
        }
    }

    /// Cloud → backend: reads frames from the broker and writes payloads to the
    /// local mTLS connection, honoring `halfClose` by finishing the write side.
    /// Errors/stream-end return; teardown is the caller's concern.
    ///
    /// Takes the raw message sequence plus a `@Sendable` frame extractor, for the
    /// same `Sendable`-capture reason as `pumpLocalToGRPC`.
    static func pumpGRPCToLocal<Messages: AsyncSequence & Sendable>(
        messages: Messages,
        frame: @Sendable (Messages.Element) -> TunnelInbound,
        write: @Sendable ([UInt8]) async throws -> Void,
        finishWrite: @Sendable () async -> Void
    ) async {
        do {
            for try await element in messages {
                let inbound = frame(element)
                if !inbound.payload.isEmpty {
                    try await write(inbound.payload)
                }
                if inbound.halfClose {
                    await finishWrite()
                    break
                }
            }
        } catch {
            // Session ended; nothing to add.
        }
    }

    // MARK: - Live implementation

    static let live = TunnelBrokerClient { config in
        let logger = Logger(label: "sh.wendy.agent.tunnel")
        guard let (host, port) = Self.splitHostPort(config.brokerURL) else {
            throw RPCError(
                code: .invalidArgument,
                message: "broker URL missing host:port: \(config.brokerURL)"
            )
        }

        // Trust roots mirror the Go agent (system roots AND the device CA).
        // `TrustRootsSource` is either system OR custom (it cannot combine them),
        // so pick by port: the cloud broker on `:443` is Google Cloud Run, which
        // terminates TLS with a public WebPKI cert (validated by the system
        // roots); a self-hosted/LAN broker presents a device-CA-signed cert
        // (validated by the device chain).
        let isCloudBroker = port == 443
        let trustRoots: TLSConfig.TrustRootsSource =
            isCloudBroker
            ? .systemDefault
            : .certificates([
                TLSConfig.CertificateSource.bytes(Array(config.chainPEM.utf8), format: .pem)
            ])

        // Hostname verification applies to every broker, whatever the port
        // (WDY-3756): a trusted root alone is not broker identity, and the
        // device presents its cert (mTLS), so it must never authenticate itself
        // to the wrong endpoint. A self-hosted/LAN broker's cert must therefore
        // carry a DNS SAN for the broker host name the agent dials.
        let serverVerification: TLSConfig.CertificateVerification = .fullVerification

        // Present the device's ECDSA leaf certificate (mTLS phase 1). Today's
        // broker runs NoClientCert and never sends a CertificateRequest, so this
        // is not transmitted on the wire; presenting it readies the device for
        // the broker to require client certs (phase 2) without a flag-day
        // cutover. Only the leaf is offered (not the ML-DSA CA chain).
        let hasKey: Bool
        switch config.keyBacking {
        case .softwarePEM(let pem): hasKey = !pem.isEmpty
        case .secureEnclave: hasKey = config.seKey != nil
        }
        let clientChain: [TLSConfig.CertificateSource]
        let clientKey: TLSConfig.PrivateKeySource?
        if !config.certificatePEM.isEmpty, hasKey {
            clientChain = [.bytes(Array(config.certificatePEM.utf8), format: .pem)]
            clientKey = try tlsPrivateKeySource(config.keyBacking, seKey: config.seKey)
        } else {
            clientChain = []
            clientKey = nil
        }
        let tls = HTTP2ClientTransport.Posix.TransportSecurity.TLS(
            certificateChain: clientChain,
            privateKey: clientKey,
            serverCertificateVerification: serverVerification,
            trustRoots: trustRoots
        )
        let transport = try HTTP2ClientTransport.Posix(
            target: .dns(host: host, port: port),
            transportSecurity: .tls(tls)
        )

        try await withGRPCClient(transport: transport) { grpc in
            let client = Wendycloud_V1_TunnelBrokerService.Client(wrapping: grpc)

            let identity = Self.identityHeader(orgID: config.orgID, assetID: config.assetID)
            var metadata = Metadata()
            metadata.addString(identity, forKey: "x-wendy-client-cert")
            metadata.addString(identity, forKey: "x-forwarded-client-cert")

            let presence = StreamingClientRequest<Wendycloud_V1_AgentHeartbeat>(
                metadata: metadata
            ) { writer in
                // Heartbeat until the call ends (which cancels this producer).
                while true {
                    try await Task.sleep(for: Self.heartbeatInterval)
                    try await writer.write(Wendycloud_V1_AgentHeartbeat())
                }
            }

            logger.info(
                "registering presence with broker",
                metadata: ["broker": "\(host):\(port)", "asset_id": "\(config.assetID)"]
            )

            try await client.registerPresence(request: presence) { [metadata] response in
                try await withThrowingTaskGroup(of: Void.self) { group in
                    for try await dial in response.messages {
                        group.addTask {
                            await Self.handleDial(
                                dial,
                                client: client,
                                config: config,
                                metadata: metadata,
                                logger: logger
                            )
                        }
                    }
                }
            }
        }
    }

    /// Serves one broker `DialRequest`: SSRF-guards the target, opens a plain TCP
    /// connection to the local mTLS port, opens an `AgentTunnel` stream, and runs
    /// the two relay pumps until the session ends.
    private static func handleDial<Transport: ClientTransport>(
        _ dial: Wendycloud_V1_DialRequest,
        client: Wendycloud_V1_TunnelBrokerService.Client<Transport>,
        config: Config,
        metadata: Metadata,
        logger: Logger
    ) async {
        guard Self.isLoopback(dial.host) else {
            logger.error(
                "broker dial request rejected: only loopback targets allowed",
                metadata: ["host": "\(dial.host)"]
            )
            return
        }
        let port = Self.remapPort(requested: Int(dial.port), mtlsPort: config.mtlsPort)
        let sessionID = dial.sessionID

        do {
            let local = try await ClientBootstrap(group: .singletonMultiThreadedEventLoopGroup)
                .channelOption(ChannelOptions.allowRemoteHalfClosure, value: true)
                .connect(host: dial.host, port: port) { channel in
                    channel.eventLoop.makeCompletedFuture {
                        try NIOAsyncChannel<ByteBuffer, ByteBuffer>(
                            wrappingChannelSynchronously: channel
                        )
                    }
                }

            logger.info(
                "relaying tunnel session",
                metadata: ["session_id": "\(sessionID)", "port": "\(port)"]
            )

            try await local.executeThenClose { inbound, outbound in
                let request = StreamingClientRequest<Wendycloud_V1_TunnelData>(
                    metadata: metadata
                ) { writer in
                    await Self.pumpLocalToGRPC(
                        sessionID: sessionID,
                        inbound: inbound,
                        bytes: { Array($0.readableBytesView) }
                    ) { event in
                        var message = Wendycloud_V1_TunnelData()
                        switch event {
                        case .join(let sid): message.sessionID = sid
                        case .data(let bytes): message.payload = Data(bytes)
                        case .halfClose: message.halfClose = true
                        }
                        try await writer.write(message)
                    }
                }
                try await client.agentTunnel(request: request) { response in
                    await Self.pumpGRPCToLocal(
                        messages: response.messages,
                        frame: {
                            TunnelInbound(payload: Array($0.payload), halfClose: $0.halfClose)
                        },
                        write: { try await outbound.write(ByteBuffer(bytes: $0)) },
                        finishWrite: { outbound.finish() }
                    )
                }
            }
        } catch {
            logger.error(
                "tunnel session failed",
                metadata: ["session_id": "\(sessionID)", "error": "\(error)"]
            )
        }
    }
}
