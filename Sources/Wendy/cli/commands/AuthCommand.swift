import ArgumentParser
import Crypto
import Foundation
import Noora
import SwiftASN1
import WendyCloudGRPC
import WendySDK
import X509

struct AuthCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "auth",
        abstract: "Managed authentication to cloud services",
        shouldDisplay: false,
        subcommands: [
            LoginCommand.self,
            LogoutCommand.self,
            RefreshCertsCommand.self,
        ]
    )
}

struct LoginCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "login",
        abstract: "Log into cloud services"
    )

    @Option
    var cloudDashboard = "https://cloud.wendy.sh"

    @Option
    var cloudGRPC = "cloud.wendy.sh"

    func run() async throws {
        try await loginFlow(
            cloudDashboard: cloudDashboard,
            cloudGRPC: cloudGRPC
        ) { token in
            Noora().success("Logged in")
            #if canImport(Darwin)
                Task {
                    try await Task.sleep(for: .seconds(1))
                    Darwin.exit(0)
                }
            #endif
        }
    }
}

struct RefreshCertsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "refresh-certs",
        abstract: "Refresh the development certificates for your CLI"
    )

    func run() async throws {
        try await withAuth(title: "Refresh certificates") { auth in
            try await withCloudGRPCClient(auth: auth) { client in
                let certs = Wendycloud_V1_CertificateService.Client(wrapping: client.grpc)

                var auth = auth
                for (index, cert) in auth.certificates.enumerated() {
                    let privateKey = Certificate.PrivateKey(P256.Signing.PrivateKey())
                    let issued = try await withCSR(
                        userId: cert.userID,
                        forOrganizationId: cert.organizationID,
                        privateKey: privateKey
                    ) { csr in
                        try await certs.refreshCertificate(
                            .with {
                                $0.pemCsr = try csr.serializeAsPEM().pemString
                            }
                        )
                    }
                    let newCert = try Certificate(pemEncoded: issued.certificate.pemCertificate)
                    let newCertificateChainPEM = try PEMDocument.parseMultiple(
                        pemString: issued.certificate.pemCertificateChain
                    )
                    let newCertificateChain =
                        try [newCert]
                        + newCertificateChainPEM.map { pem in
                            return try Certificate(pemDocument: pem)
                        }

                    auth.certificates[index] = try Config.Auth.Certificates(
                        organizationID: cert.organizationID,
                        userID: cert.userID,
                        privateKeyPEM: privateKey.serializeAsPEM().pemString,
                        certificateChainPEM: newCertificateChain.map {
                            try $0.serializeAsPEM().pemString
                        }
                    )
                }
                var config = try getConfig()
                config.addAuth(auth)
                try config.save()
                Noora().success("Refreshed certificates")
            }
        }
    }
}

struct EnrollmentTokenRule: ValidatableRule {
    var error: ValidatableError {
        "Code must be 6 alphanumeric characters"
    }

    func validate(input: String) -> Bool {
        input.count >= 6 && !input.contains(where: \.isWhitespace)
    }
}

struct LogoutCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "logout",
        abstract: "Log out of cloud services"
    )

    func run() async throws {
        var config = try getConfig()

        if config.auth.isEmpty {
            Noora().error("No accounts found")
            return
        }

        let logout = Noora().singleChoicePrompt(
            title: "Logout",
            question: "Which account do you want to log out of?",
            options: config.auth
        )

        config.auth.removeAll { $0 == logout }

        let data = try JSONEncoder().encode(config)
        try data.write(to: configURL)
    }
}
