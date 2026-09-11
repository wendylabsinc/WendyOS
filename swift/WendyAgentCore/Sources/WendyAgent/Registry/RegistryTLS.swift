import Foundation
import HummingbirdTLS
import Logging
import NIOCore
import NIOSSL

/// Builds the TLS server configuration for the registry's push listener from
/// the device's provisioned identity, mirroring the main gRPC server's mTLS
/// setup (`WendyAgent.mTLSSecurity`): serve `[leaf, chain]`, demand a client
/// certificate, and fully replace BoringSSL's chain validation with
/// `ClientCertAuthorizer` (trust-root path verification + org-enforcement
/// policy, failing closed).
enum RegistryTLS {
    /// Plain-`Sendable` inputs captured at configuration time; PEM parsing (and
    /// its failure modes) happens later in `channelConfiguration`, inside the
    /// listener's `run()`, where errors surface in the listener's error path.
    struct Configuration: Sendable {
        var certPEM: String
        var chainPEM: String
        var keyBacking: ProvisioningStore.KeyBacking
        var seKey: SEPrivateKey?
        var deviceScope: OrgIdentity.Scope?
        var orgMode: ClientCertAuthorizer.OrgEnforcementMode
    }

    /// Derives the verification policy exactly like the gRPC server: the
    /// device's tenant comes from its own leaf certificate (nil fails closed in
    /// `ClientCertAuthorizer`), and the enforcement mode comes from
    /// `WENDY_MTLS_ORG_ENFORCEMENT` (off|grace|strict, defaulting to grace).
    static func makeConfiguration(
        certs: ProvisioningService.ProvisioningCerts,
        environment: [String: String] = ProcessInfo.processInfo.environment,
        logger: Logger
    ) -> Configuration {
        let deviceScope = ClientCertAuthorizer.scope(fromLeafPEM: certs.certPEM)
        if deviceScope == nil {
            logger.error(
                "Could not determine device tenant from its own certificate; the registry push listener will reject all clients (fail closed). Re-provision the device to recover."
            )
        }
        let rawOrgEnforcement = environment["WENDY_MTLS_ORG_ENFORCEMENT"]
        let (orgMode, recognized) = ClientCertAuthorizer.OrgEnforcementMode.parse(rawOrgEnforcement)
        if !recognized {
            logger.warning(
                "Unrecognized WENDY_MTLS_ORG_ENFORCEMENT value; defaulting to grace",
                metadata: ["value": "\(rawOrgEnforcement ?? "")"]
            )
        }
        return Configuration(
            certPEM: certs.certPEM,
            chainPEM: certs.chainPEM,
            keyBacking: certs.keyBacking,
            seKey: certs.seKey,
            deviceScope: deviceScope,
            orgMode: orgMode
        )
    }

    /// Builds the Hummingbird TLS channel configuration. Throws on unparseable
    /// PEM — callers must treat that as "listener stays down", never as a
    /// license to fall back to plain HTTP on a non-loopback interface.
    ///
    /// `certificateVerification = .noHostnameVerification` (rather than
    /// `.noVerification`) is load-bearing: it requires the client to present a
    /// certificate, and NIOSSL only invokes `customVerificationCallback` when
    /// verification is not disabled. The callback fully REPLACES BoringSSL's
    /// chain validation, so `ClientCertAuthorizer` performs the complete
    /// verification itself and fails closed.
    static func channelConfiguration(_ config: Configuration) throws -> TLSChannelConfiguration {
        let leafCerts = try NIOSSLCertificate.fromPEMBytes(Array(config.certPEM.utf8))
        let chainCerts = try NIOSSLCertificate.fromPEMBytes(Array(config.chainPEM.utf8))
        guard let leaf = leafCerts.first else {
            throw NIOSSLError.failedToLoadCertificate
        }
        let key: NIOSSLPrivateKey
        switch config.keyBacking {
        case .softwarePEM(let pem):
            key = try NIOSSLPrivateKey(bytes: Array(pem.utf8), format: .pem)
        case .secureEnclave:
            guard let seKey = config.seKey else {
                throw TLSKeySourceError.missingSecureEnclaveKey
            }
            key = NIOSSLPrivateKey(customPrivateKey: seKey)
        }

        var tls = TLSConfiguration.makeServerConfiguration(
            certificateChain: ([leaf] + leafCerts.dropFirst() + chainCerts).map {
                .certificate($0)
            },
            privateKey: .privateKey(key)
        )
        tls.minimumTLSVersion = .tlsv12
        tls.certificateVerification = .noHostnameVerification
        tls.trustRoots = .certificates(chainCerts)

        let trustRootsPEM = config.chainPEM
        let deviceScope = config.deviceScope
        let orgMode = config.orgMode
        return TLSChannelConfiguration(
            tlsConfiguration: tls,
            customVerificationCallback: { peerCertificates, promise in
                let ders = peerCertificates.compactMap { try? $0.toDERBytes() }
                Task {
                    let authorized = await ClientCertAuthorizer.isAuthorized(
                        peerCertificatesDER: ders,
                        trustRootsPEM: trustRootsPEM,
                        deviceScope: deviceScope,
                        mode: orgMode
                    )
                    promise.succeed(authorized ? .certificateVerified : .failed)
                }
            }
        )
    }
}
