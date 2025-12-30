import Analytics
import Crypto
import Foundation
import Hummingbird
import JWTKit
import Noora
import SwiftASN1
import Synchronization
import WendyCloudGRPC
import WendySDK
import X509

#if os(macOS)
    import AppKit
    import Darwin
#endif

struct TokenWithSubject: JWTPayload {
    let sub: String

    func verify(using algorithm: some JWTKit.JWTAlgorithm) async throws {}
}

public struct Config: Sendable, Codable {
    public struct Auth: Sendable, Codable, Hashable, CustomStringConvertible {
        public let cloudDashboard: String
        public let cloudGRPC: String
        public var certificates: [Certificates]

        public struct Certificates: Sendable, Codable, Hashable {
            public let organizationID: Int32
            public let userID: String
            public let privateKeyPEM: String
            public let certificateChainPEM: [String]
        }

        public var description: String {
            certificates.first?.userID ?? "Unknown user"
        }
    }

    public var auth: [Auth]
    public var analytics: WendyAnalyticsConfig
    public var defaultDevice: String?

    public init() {
        self.auth = []
        self.defaultDevice = nil
        self.analytics = WendyAnalyticsConfig()
    }

    public mutating func addAuth(_ newAuth: Auth) {
        self.auth.removeAll {
            $0.cloudDashboard == newAuth.cloudDashboard
                && $0.cloudGRPC == newAuth.cloudGRPC
        }
        self.auth.append(newAuth)
    }

    public func save() throws {
        let data = try JSONEncoder().encode(self)
        try data.write(to: configURL)
    }
}

var configURL: URL {
    get throws {
        let wendyURL = FileManager.default
            .homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy")

        try FileManager.default.createDirectory(at: wendyURL, withIntermediateDirectories: true)

        return wendyURL.appendingPathComponent("config.json")
    }
}

func getConfig() -> Config {
    do {
        let data = try Data(contentsOf: configURL)
        return try JSONDecoder().decode(Config.self, from: data)
    } catch {
        return Config()
    }
}

func authenticate<R: Sendable>(
    title: TerminalText,
    forOrganizationId orgId: Int32? = nil,
    perform: @Sendable @escaping (Config.Auth) async throws -> R
) async throws -> R {
    var cloudDashboard = Noora().textPrompt(
        title: title,
        prompt: "Enter the cloud dashboard URL",
        collapseOnAnswer: false
    )

    var cloudGRPC = Noora().textPrompt(
        title: title,
        prompt: "Enter the cloud gRPC URL",
        collapseOnAnswer: false
    )

    // TODO: Add organisation ID preference to dashboard
    if cloudDashboard.isEmpty {
        cloudDashboard = "https://cloud.wendy.sh"
    } else if !cloudDashboard.contains("://") {
        cloudDashboard = "https://" + cloudDashboard
    }

    if cloudGRPC.isEmpty {
        cloudGRPC = "cloud.wendy.sh"
    }

    return try await loginFlow(
        cloudDashboard: cloudDashboard,
        cloudGRPC: cloudGRPC,
        withAuth: perform
    )
}

func withCertificates<R: Sendable>(
    title: TerminalText,
    forOrganizationId orgId: Int32,
    perform: @Sendable @escaping (Config.Auth.Certificates) async throws -> R
) async throws -> R {
    let config = getConfig()

    for auth in config.auth {
        for certificate in auth.certificates {
            if certificate.organizationID == orgId {
                return try await perform(certificate)
            }
        }
    }

    return try await authenticate(title: title, forOrganizationId: orgId) { auth in
        guard let certificate = auth.certificates.first(where: { $0.organizationID == orgId })
        else {
            throw RPCError(
                code: .aborted,
                message: "No certificate found for organization \(orgId)"
            )
        }
        return try await perform(certificate)
    }
}

func withAuth<R: Sendable>(
    title: TerminalText,
    perform: @Sendable @escaping (Config.Auth) async throws -> R
) async throws -> R {
    let config = getConfig()

    if config.auth.isEmpty {
        return try await authenticate(title: title, perform: perform)
    } else if config.auth.count == 1 {
        return try await perform(config.auth[0])
    } else {
        let account = Noora().singleChoicePrompt(
            title: title,
            question: "Which account do you want to use?",
            options: config.auth
        )
        return try await perform(account)
    }
}

func withCSR<R: Sendable>(
    userId: String,
    forOrganizationId orgId: Int32,
    privateKey: Certificate.PrivateKey,
    perform: @Sendable @escaping (CertificateSigningRequest) async throws -> R
) async throws -> R {
    let cliId = UUID().uuidString
    return try await perform(
        try CertificateSigningRequest(
            version: .v1,
            subject: DistinguishedName {
                CommonName("sh")
                CommonName("wendy")
                CommonName(userId)
                CommonName(cliId)
            },
            privateKey: privateKey,
            attributes: CertificateSigningRequest.Attributes([
                CertificateSigningRequest.Attribute(
                    ExtensionRequest(
                        extensions: .init {
                            Critical(
                                SubjectAlternativeNames([
                                    .uniformResourceIdentifier("urn:wendy:org:\(orgId)"),
                                    .uniformResourceIdentifier(
                                        "urn:wendy:org:\(orgId):user:\(userId)"
                                    ),
                                ])
                            )
                        }
                    )
                )
            ])
        )
    )
}

func setupConfig(
    enrollmentToken: String,
    userId: String,
    organizationId: Int32,
    cloudDashboard: String,
    cloudGRPC: String
) async throws -> Config.Auth {
    var config = getConfig()

    let endpoint = AgentConnectionOptions.Endpoint(
        host: cloudGRPC,
        port: 50051
    )
    let auth = try await withGRPCClient(
        endpoint,
        security: .plaintext
    ) { client in
        let client = CloudGRPCClient(
            grpc: client,
            cloudHost: cloudGRPC,
            metadata: [:]
        )
        let certs = Wendycloud_V1_CertificateService.Client(wrapping: client.grpc)

        let privateKey = Certificate.PrivateKey(P256.Signing.PrivateKey())
        let issued = try await withCSR(
            userId: userId,
            forOrganizationId: organizationId,
            privateKey: privateKey
        ) { csr in
            try await certs.issueCertificate(
                .with {
                    $0.enrollmentToken = enrollmentToken
                    $0.pemCsr = try csr.serializeAsPEM().pemString
                },
                metadata: client.metadata
            )
        }

        if issued.hasError {
            throw RPCError(code: .aborted, message: issued.error.message)
        }

        let cert = try Certificate(pemEncoded: issued.certificate.pemCertificate)
        let certificateChainPEM = try PEMDocument.parseMultiple(
            pemString: issued.certificate.pemCertificateChain
        )
        let certificateChain =
            try [cert]
            + certificateChainPEM.map { pem in
                return try Certificate(pemDocument: pem)
            }

        return Config.Auth(
            cloudDashboard: cloudDashboard,
            cloudGRPC: cloudGRPC,
            certificates: [
                try .init(
                    organizationID: organizationId,
                    userID: userId,
                    privateKeyPEM: try privateKey.serializeAsPEM().pemString,
                    certificateChainPEM: certificateChain.map { try $0.serializeAsPEM().pemString }
                )
            ]
        )
    }
    config.addAuth(auth)

    let data = try JSONEncoder().encode(config)
    try data.write(to: configURL)
    return auth
}

func loginFlow<R: Sendable>(
    cloudDashboard: String,
    cloudGRPC: String,
    withAuth: @Sendable @escaping (Config.Auth) async throws -> R
) async throws -> R {
    let isFinished = Mutex(false)
    let (stream, continuation) = AsyncStream<R>.makeStream()
    let router = Router().get("cli-callback") { req, context in
        do {
            let enrollmentToken = try req.uri.queryParameters.require("token")
            let userId = try req.uri.queryParameters.require("user_id")
            let organizationId = try req.uri.queryParameters.require("org_id", as: Int32.self)

            let auth = try await setupConfig(
                enrollmentToken: enrollmentToken,
                userId: userId,
                organizationId: organizationId,
                cloudDashboard: cloudDashboard,
                cloudGRPC: cloudGRPC
            )
            continuation.yield(try await withAuth(auth))
            continuation.finish()
            isFinished.withLock { $0 = true }

            return Response(
                status: .ok,
                body: ResponseBody(byteBuffer: ByteBuffer(string: "Enrolled!"))
            )
        } catch {
            return Response(
                status: .badRequest,
                body: ResponseBody(byteBuffer: ByteBuffer(string: "Provisioning failed: \(error)"))
            )
        }
    }

    var server = Application(
        router: router,
        configuration: .init(
            address: .hostname("127.0.0.1", port: 0)
        ),
        onServerRunning: { channel in
            let port = channel.localAddress!.port!
            let url =
                "\(cloudDashboard)/cli-auth?redirect_uri=http://localhost:\(port)/cli-callback"
            #if os(macOS)
                if NSWorkspace.shared.open(URL(string: url)!) {
                    Noora().info(
                        """
                        Open the following link in your browser:
                        > \(cloudDashboard)/cli-auth?redirect_uri=http://localhost:\(port)/cli-callback
                        """
                    )
                    return
                }
            #endif

            //         repeat {
            //             let enrollmentToken = Noora().textPrompt(
            //                 title: "Provide the enrollment token",
            //                 prompt: "Enter token",
            //                 collapseOnAnswer: false,
            //                 validationRules: [
            //                     EnrollmentTokenRule()
            //                 ]
            //             )
            //             .trimmingCharacters(in: .whitespacesAndNewlines)

            //             do {
            //                 let auth = try await setupConfig(
            //                     enrollmentToken: enrollmentToken,
            //                     userId: userId,
            //                     organizationId: organizationId,
            //                     cloudDashboard: cloudDashboard,
            //                     cloudGRPC: cloudGRPC
            //                 )
            //                 continuation.yield(try await withAuth(auth))
            //                 continuation.finish()
            //             } catch {
            //                 Noora().error("Failed to setup config: \(error)")
            //             }
            //         } while !isFinished.withLock(\.self)
        }
    )

    server.logger.logLevel = .critical

    return try await withThrowingTaskGroup { group in
        group.addTask { [server] in
            try await server.runService()
        }

        for try await result in stream {
            group.cancelAll()
            return result
        }

        group.cancelAll()
        throw CancellationError()
    }
}
