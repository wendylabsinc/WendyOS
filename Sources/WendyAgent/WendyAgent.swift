import ArgumentParser
import AsyncHTTPClient
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCServiceLifecycle
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
            #if DEBUG
                let defaultLogLevel = Logger.Level.debug
            #else
                let defaultLogLevel = Logger.Level.info
            #endif

            let level =
                ProcessInfo.processInfo.environment["LOG_LEVEL"]
                .flatMap(Logger.Level.init) ?? defaultLogLevel

            var logger = StreamLogHandler.standardError(label: label)
            logger.logLevel = level
            return logger
        }

        let logger = Logger(label: "sh.wendy.agent")

        logger.info("Starting Wendy Agent version \(Version.current) on port \(port)")

        // Clean up old backup files from previous successful updates
        await cleanupOldBackupFiles(logger: logger)

        let (signal, continuation) = AsyncStream<Void>.makeStream()

        let provisioning: WendyProvisioningService
        let mTLS: HTTP2ServerTransport.Posix.TransportSecurity?
        let config: any AgentConfigService = try await {
            try await FileSystemAgentConfigService(directory: FilePath(configDir))
        }()

        var backgroundServices: [any ServiceLifecycle.Service] = [
            ContainerMonitor.shared  // Add container monitor as a background service
        ]
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

        var services = [any Service]()
        for service in backgroundServices {
            services.append(service)
        }
        for server in servers {
            services.append(server)
        }
        if mTLS == nil {
            logger.info("Adding Registry Container Service for development")
            services.append(RegistryContainerService())
        }

        var serviceGroupConfig = ServiceGroupConfiguration(
            services: services,
            logger: logger
        )
        serviceGroupConfig.maximumGracefulShutdownDuration = .seconds(10)
        let serviceGroup = ServiceGroup(
            configuration: serviceGroupConfig
        )

        try await withThrowingTaskGroup(of: Void.self) { taskGroup in
            taskGroup.addTask {
                try await serviceGroup.run()
            }

            taskGroup.addTask {
                for try await () in signal {
                    logger.info("Received signal, restarting")
                    try await Task.sleep(for: .seconds(3))
                    return
                }
            }

            defer { taskGroup.cancelAll() }
            try await taskGroup.next()
        }
    }
}

/// Cleans up old backup files from previous successful updates
/// Keeps the most recent .backup file only if it's from a recent update (< 48 hours old)
/// This prevents accumulation of backup files while maintaining a safety net
/// Also handles automatic recovery if current binary is missing (e.g., power loss during update)
func cleanupOldBackupFiles(logger: Logger) async {
    let filesystem = FileSystem.shared
    let currentBinaryPath = FilePath(ProcessInfo.processInfo.arguments[0])
    let backupPath = currentBinaryPath.appending(".backup")

    do {
        // Check if current binary exists
        let currentInfo = try await filesystem.info(forFileAt: currentBinaryPath)

        // Check if backup exists
        guard let backupInfo = try await filesystem.info(forFileAt: backupPath) else {
            // No backup file exists, nothing to clean up
            return
        }

        // If we get here, both current binary and backup exist
        guard let currentFileInfo = currentInfo else {
            // Current binary is missing but backup exists - RECOVERY MODE
            logger.warning(
                "Current binary missing but backup exists - attempting automatic recovery",
                metadata: [
                    "current_path": "\(currentBinaryPath)",
                    "backup_path": "\(backupPath)",
                ]
            )

            do {
                // Restore from backup
                try await filesystem.moveItem(at: backupPath, to: currentBinaryPath)
                logger.info(
                    "Successfully recovered binary from backup",
                    metadata: [
                        "restored_to": "\(currentBinaryPath)"
                    ]
                )
                // After recovery, no backup remains so we're done
                return
            } catch {
                logger.critical(
                    "Failed to recover binary from backup - system may be broken",
                    metadata: [
                        "error": "\(error)",
                        "backup_path": "\(backupPath)",
                    ]
                )
                // Don't delete backup if recovery failed
                return
            }
        }

        // Check if backup is older than current binary (indicates successful update)
        if backupInfo.lastDataModificationTime.seconds
            < currentFileInfo.lastDataModificationTime.seconds
        {
            // Calculate age of backup
            let backupAge = Date().timeIntervalSince(
                Date(
                    timeIntervalSince1970: TimeInterval(backupInfo.lastDataModificationTime.seconds)
                )
            )

            // Keep backup for 48 hours as safety net
            if backupAge > (48 * 3600) {
                logger.info(
                    "Removing old backup file from successful update",
                    metadata: [
                        "path": "\(backupPath)",
                        "age_hours": "\(Int(backupAge / 3600))",
                    ]
                )
                try await filesystem.removeItem(at: backupPath)
            } else {
                logger.debug(
                    "Keeping recent backup file",
                    metadata: [
                        "path": "\(backupPath)",
                        "age_hours": "\(Int(backupAge / 3600))",
                    ]
                )
            }
        } else {
            // Backup is newer than current binary - this is unusual
            // Keep it for manual inspection/recovery
            logger.warning(
                "Backup file is newer than current binary - keeping for manual inspection",
                metadata: [
                    "backup_path": "\(backupPath)",
                    "backup_modified": "\(backupInfo.lastDataModificationTime)",
                    "current_modified": "\(currentFileInfo.lastDataModificationTime)",
                ]
            )
        }
    } catch {
        logger.warning(
            "Failed to clean up backup files",
            metadata: [
                "error": "\(error)"
            ]
        )
    }
}
