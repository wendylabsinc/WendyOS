import ArgumentParser
import AsyncHTTPClient
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOSSL
import ServiceLifecycle
import WendyAgentGRPC
import WendyCloudGRPC
import WendyShared
import X509
import _NIOFileSystem

@main
struct WendyAgent: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "wendy-agent",
        abstract: "Wendy Agent",
        version: Version.current
    )

    @Option(name: .shortAndLong, help: "The port to listen on for incoming connections.")
    var port: Int = 50051

    @Option(name: .shortAndLong, help: "The directory to store configuration files in.")
    var configDir: String = "/etc/wendy-agent"

    func run() async throws {
        LoggingSystem.bootstrap { label in
            let level =
                ProcessInfo.processInfo.environment["LOG_LEVEL"]
                .flatMap(Logger.Level.init) ?? .info

            var logger = StreamLogHandler.standardError(label: label)
            logger.logLevel = level
            return logger
        }

        let logger = Logger(label: "sh.wendy.agent")

        logger.info("Starting Wendy Agent version \(Version.current) on port \(port)")

        let (signal, continuation) = AsyncStream<Void>.makeStream()

        let provisioning: WendyProvisioningService
        let mTLS: HTTP2ServerTransport.Posix.TransportSecurity?
        let config: any AgentConfigService = try await {
            try await FileSystemAgentConfigService(directory: FilePath(configDir))
        }()

        var backgroundServices: [any ServiceLifecycle.Service] = []
        var servers = [GRPCServer<HTTP2ServerTransport.Posix>]()

        if let enrolled = await config.enrolled {
            provisioning = await WendyProvisioningService(
                privateKey: config.privateKey,
                enrolled: enrolled
            )
            mTLS = try await .mTLS(
                certificateChain: enrolled.certificateChainPEM.map { cert in
                    return TLSConfig.CertificateSource.bytes(Array(cert.utf8), format: .pem)
                },
                privateKey: .bytes(
                    Array(config.privateKey.serializeAsPEM().pemString.utf8),
                    format: .pem
                )
            ) { tls in
                tls.clientCertificateVerification = .noHostnameVerification
                tls.customVerificationCallback = { certs, promise in
                    guard
                        let cert = certs.first,
                        cert._subjectAlternativeNames().contains(where: { name in
                            name.contents.contains("urn:wendy:org:\(enrolled.organizationId)".utf8)
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
            let cloudClient = try await CloudClient(
                enrolled: enrolled,
                privateKey: config.privateKey
            )
            backgroundServices.append(cloudClient)
            // TODO: Also set up OTel on 4318
            servers.append(
                GRPCServer(
                    transport: HTTP2ServerTransport.Posix(
                        address: .ipv4(host: "127.0.0.1", port: 4317),
                        transportSecurity: .plaintext
                    ),
                    services: [
                        OpenTelemetryProxy(cloud: cloudClient)
                    ]
                )
            )
        } else {
            logger.notice("Agent requires provisioning")
            mTLS = nil
            provisioning = await WendyProvisioningService(
                privateKey: config.privateKey
            ) { enrolled in
                // TODO: Save to disk and restart server
                try await config.provisionCertificateChain(
                    enrolled: enrolled
                )
                logger.notice("Provisioning complete. Restarting server")
                continuation.yield()
            }
        }

        let authenticatedServices: [any GRPCCore.RegistrableRPCService] = [
            WendyContainerService(),
            WendyAgentService(shouldRestart: {
                print("Shutting down server")
                continuation.yield()
            }),
            provisioning,
        ]
        let unauthenticatedServices: [any GRPCCore.RegistrableRPCService] = [
            provisioning
        ]

        let plaintextServices = mTLS == nil ? authenticatedServices : unauthenticatedServices

        if let mTLS {
            servers.append(
                GRPCServer(
                    transport: HTTP2ServerTransport.Posix(
                        address: .ipv4(host: "0.0.0.0", port: port + 1),
                        transportSecurity: mTLS
                    ),
                    services: authenticatedServices,
                    interceptors: [
                        WendyErrorInterceptor()
                    ]
                )
            )
            servers.append(
                GRPCServer(
                    transport: HTTP2ServerTransport.Posix(
                        address: .ipv6(host: "::", port: port + 1),
                        transportSecurity: mTLS
                    ),
                    services: authenticatedServices,
                    interceptors: [
                        WendyErrorInterceptor()
                    ]
                )
            )
        }

        servers.append(
            GRPCServer(
                transport: HTTP2ServerTransport.Posix(
                    address: .ipv4(host: "0.0.0.0", port: port),
                    transportSecurity: .plaintext
                ),
                services: plaintextServices,
                interceptors: [
                    WendyErrorInterceptor()
                ]
            )
        )

        servers.append(
            GRPCServer(
                transport: HTTP2ServerTransport.Posix(
                    address: .ipv6(host: "::", port: port),
                    transportSecurity: .plaintext
                ),
                services: plaintextServices,
                interceptors: [
                    WendyErrorInterceptor()
                ]
            )
        )

        try await withThrowingTaskGroup(of: Void.self) { taskGroup in
            for server in servers {
                taskGroup.addTask {
                    try await server.serve()
                    continuation.finish()
                }
            }

            defer {
                for server in servers {
                    server.beginGracefulShutdown()
                }
                taskGroup.cancelAll()
            }

            for service in backgroundServices {
                taskGroup.addTask {
                    try await service.run()
                }
            }

            // taskGroup.addTask {
            //     do {
            //         let imageName = "hello-python"
            //         try await PullImage().pullAndRun(
            //             image: "cloud-c7e56/agent-test/\(imageName):latest",
            //             appName: imageName,
            //             labels: [:],
            //             bearerToken: "...",
            //             registry: "us-central1-docker.pkg.dev"
            //         )
            //     } catch {
            //         logger.error("Failed to pull and run image: \(error)")
            //     }
            // }

            for try await () in signal {
                logger.info("Received signal, restarting")
                try await Task.sleep(for: .seconds(3))
                return
            }
        }
    }
}
