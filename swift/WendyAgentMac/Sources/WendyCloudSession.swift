import Foundation
import WendyAgentCore

struct CloudDiscoverySession: Equatable, Sendable {
    let cloudGRPC: String
    let credentials: WendyCloudCredentials
}

/// Loads the same identity selected by non-interactive Wendy CLI cloud commands.
enum WendyCLICloudConfig {
    struct Configuration: Decodable {
        let auth: [AuthEntry]
        let defaultCloudGRPC: String?
        let defaultOrgId: Int32?
    }

    struct AuthEntry: Decodable {
        let cloudGRPC: String
        let certificates: [Certificate]
    }

    struct Certificate: Decodable {
        let pemCertificate: String
        let pemCertificateChain: String
        let pemPrivateKey: String
        let organizationId: Int32
        let userId: String?
    }

    static func loadSession(from url: URL) -> CloudDiscoverySession? {
        guard let data = try? Data(contentsOf: url),
            let configuration = try? JSONDecoder().decode(Configuration.self, from: data)
        else {
            return nil
        }
        return resolveSession(in: configuration)
    }

    static func resolveSession(in configuration: Configuration) -> CloudDiscoverySession? {
        let auth: AuthEntry?
        if configuration.auth.count == 1 {
            auth = configuration.auth[0]
        } else if let organizationID = configuration.defaultOrgId,
            organizationID != 0
        {
            auth = configuration.auth.first {
                $0.certificates.first?.organizationId == organizationID
            }
        } else if let endpoint = configuration.defaultCloudGRPC {
            auth = configuration.auth.first { $0.cloudGRPC == endpoint }
        } else {
            auth = nil
        }

        guard let auth, let certificate = auth.certificates.first else { return nil }
        return CloudDiscoverySession(
            cloudGRPC: auth.cloudGRPC,
            credentials: WendyCloudCredentials(
                pemCertificate: certificate.pemCertificate,
                pemCertificateChain: certificate.pemCertificateChain,
                pemPrivateKey: certificate.pemPrivateKey,
                organizationID: certificate.organizationId,
                userID: certificate.userId
            )
        )
    }
}

struct LiveCloudSessionSource: Sendable {
    let configurationURL: URL

    init(
        configurationURL: URL = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy/config.json")
    ) {
        self.configurationURL = configurationURL
    }

    func load() -> CloudDiscoverySession? {
        WendyCLICloudConfig.loadSession(from: configurationURL)
    }
}

enum MeshDirectorySync {
    static func refresh(session: CloudDiscoverySession) async throws -> WendyMeshDirectory {
        let devices = try await WendyCloudDirectory.listOnlineDevices(
            cloudGRPC: session.cloudGRPC,
            credentials: session.credentials
        )
        return WendyMeshDirectory(
            devices: devices.map {
                WendyMeshDevice(
                    assetID: $0.id,
                    name: $0.name,
                    organizationID: $0.organizationID,
                    online: true
                )
            }
        )
    }
}
