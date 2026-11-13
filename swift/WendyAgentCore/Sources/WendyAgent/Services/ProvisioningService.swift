import Foundation
import GRPCCore
import Logging
import NIOSSL
import SwiftASN1
import WendyAgentGRPC
import X509

/// Real device provisioning for the macOS agent: generates a device identity,
/// exchanges a CSR with the cloud (legacy) or pki-core's ACME frontend (direct
/// PKI) for a signed certificate, persists the enrollment, and reports state.
/// Mirrors the Go agent's ProvisioningService.
actor ProvisioningService: Wendy_Agent_Services_V1_WendyProvisioningService.SimpleServiceProtocol {
    typealias ACMEEnroll =
        @Sendable (
            _ config: ACMEEnrollmentConfig,
            _ accountKeyPath: URL,
            _ csrDER: [UInt8],
            _ devicePublicKey: Certificate.PublicKey
        ) async throws -> (certPEM: String, chainPEM: String)

    struct ProvisioningInfo: Sendable {
        var cloudHost: String
        var orgID: Int32
        var assetID: Int32
        var enrolled: Bool
        /// Set for direct PKI (ACME) enrollment; empty for legacy Cloud identities.
        var principalURI: String
    }

    struct ProvisioningCerts: Sendable {
        var certPEM: String
        var chainPEM: String
        var keyBacking: ProvisioningStore.KeyBacking
        var seKey: SEPrivateKey?
    }

    private let store: ProvisioningStore
    private let configPath: URL
    private let cloudClient: CloudCertificateClient
    private let acmeEnroll: ACMEEnroll
    /// Overridable so tests can force the SE / software branch deterministically
    /// regardless of the host's actual hardware. Defaults to the real check.
    private let isSecureEnclaveAvailable: @Sendable () -> Bool
    /// Why this host cannot take a direct PKI enrollment, or nil when it can.
    /// Overridable for tests; defaults to `hostACMEEnrollmentUnavailableReason`.
    let acmeEnrollmentUnavailableReason: @Sendable () -> String?
    private let logger = Logger(label: "sh.wendy.agent.provisioning")

    private var enrolled = false
    private var cloudHost = ""
    private var orgID: Int32 = 0
    private var assetID: Int32 = 0
    private var keyBacking: ProvisioningStore.KeyBacking = .softwarePEM("")
    private var seKey: SEPrivateKey?
    private var certPEM = ""
    private var chainPEM = ""
    private var principalURI = ""
    /// The actor is reentrant across the network await; this keeps a second
    /// enrollment from starting while one is in flight (Go holds its mutex).
    private var provisioningInFlight = false

    private var onProvisioned: (@Sendable (ProvisioningCerts) async -> Void)?
    private var onUnprovisioned: (@Sendable () async -> Void)?

    init(
        configPath: URL,
        cloudClient: CloudCertificateClient = .live,
        acmeEnroll: @escaping ACMEEnroll = {
            try await ACMEEnrollment.enroll($0, accountKeyPath: $1, csrDER: $2, devicePublicKey: $3)
        },
        isSecureEnclaveAvailable: @Sendable @escaping () -> Bool = {
            SecureEnclaveIdentity.isAvailable
        },
        acmeEnrollmentUnavailableReason: @Sendable @escaping () -> String? = {
            ProvisioningService.hostACMEEnrollmentUnavailableReason()
        }
    ) {
        self.store = ProvisioningStore(configPath: configPath)
        self.configPath = configPath
        self.cloudClient = cloudClient
        self.acmeEnroll = acmeEnroll
        self.isSecureEnclaveAvailable = isSecureEnclaveAvailable
        self.acmeEnrollmentUnavailableReason = acmeEnrollmentUnavailableReason
        guard let loaded = self.store.load() else { return }

        if loaded.keyBacking == .secureEnclave {
            // Fail closed: if the SE blob can't be reconstructed (e.g. the
            // Keychain item vanished, or this isn't the Mac that created it),
            // treat the device as unprovisioned rather than silently falling
            // back to no key or a software one. Re-provisioning is required.
            guard let identity = try? SecureEnclaveIdentity.load(store: KeychainStore()) else {
                self.logger.error(
                    "Provisioning state says secureEnclave but the Keychain blob could not be loaded; treating device as unprovisioned"
                )
                return
            }
            self.seKey = identity.nioCustomKey
        }

        self.enrolled = loaded.enrolled
        self.cloudHost = loaded.cloudHost
        self.orgID = loaded.orgID
        self.assetID = loaded.assetID
        self.keyBacking = loaded.keyBacking
        self.certPEM = loaded.certPEM
        self.chainPEM = loaded.chainPEM
        self.principalURI = loaded.principalURI
    }

    func setCallbacks(
        onProvisioned: (@Sendable (ProvisioningCerts) async -> Void)?,
        onUnprovisioned: (@Sendable () async -> Void)?
    ) {
        self.onProvisioned = onProvisioned
        self.onUnprovisioned = onUnprovisioned
    }

    func provisioningInfo() -> ProvisioningInfo {
        ProvisioningInfo(
            cloudHost: self.cloudHost,
            orgID: self.orgID,
            assetID: self.assetID,
            enrolled: self.enrolled,
            principalURI: self.principalURI
        )
    }

    func provisioningCerts() -> ProvisioningCerts? {
        guard self.enrolled else { return nil }
        return ProvisioningCerts(
            certPEM: self.certPEM,
            chainPEM: self.chainPEM,
            keyBacking: self.keyBacking,
            seKey: self.seKey
        )
    }

    // MARK: - RPCs

    func startProvisioning(
        request: Wendy_Agent_Services_V1_StartProvisioningRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V1_StartProvisioningResponse {
        guard !self.enrolled else {
            throw RPCError(code: .failedPrecondition, message: "agent is already provisioned")
        }
        try self.beginProvisioning()
        defer { self.provisioningInFlight = false }

        // NEVER add `request.enrollmentToken` (or any other credential) to this
        // metadata — it is a bearer secret that would then land in log
        // aggregation/SIEM. Only non-secret operational identifiers belong here.
        self.logger.info(
            "Starting provisioning",
            metadata: [
                "org_id": "\(request.organizationID)",
                "cloud_host": "\(request.cloudHost)",
                "asset_id": "\(request.assetID)",
            ]
        )

        let commonName = DeviceIdentity.commonName(
            organizationID: request.organizationID,
            assetID: request.assetID
        )
        let identityURN = DeviceIdentity.assetURN(
            organizationID: request.organizationID,
            assetID: request.assetID
        )

        let key = try self.generateDeviceKey(commonName: commonName, identityURN: identityURN)
        let keyBacking = key.backing
        let seKey = key.seKey

        let issued = try await self.cloudClient.issue(
            request.cloudHost,
            key.csrPEM,
            request.enrollmentToken
        )
        guard !issued.certPEM.isEmpty else {
            throw RPCError(code: .internalError, message: "cloud returned empty certificate")
        }

        // Persist BEFORE mutating in-memory state so a disk failure never wedges
        // the device as "already provisioned".
        do {
            try self.store.save(
                cloudHost: request.cloudHost,
                orgID: request.organizationID,
                assetID: request.assetID,
                keyBacking: keyBacking,
                certPEM: issued.certPEM,
                chainPEM: issued.chainPEM
            )
        } catch {
            self.logger.error(
                "Failed to persist provisioning state",
                metadata: ["error": "\(error)"]
            )
            throw RPCError(
                code: .internalError,
                message: "failed to save provisioning state: \(error)"
            )
        }

        self.enrolled = true
        self.cloudHost = request.cloudHost
        self.orgID = request.organizationID
        self.assetID = request.assetID
        self.keyBacking = keyBacking
        self.seKey = seKey
        self.certPEM = issued.certPEM
        self.chainPEM = issued.chainPEM

        self.logger.info(
            "Provisioning completed",
            metadata: ["org_id": "\(self.orgID)", "asset_id": "\(self.assetID)"]
        )

        if let cb = self.onProvisioned {
            let certs = ProvisioningCerts(
                certPEM: self.certPEM,
                chainPEM: self.chainPEM,
                keyBacking: self.keyBacking,
                seKey: self.seKey
            )
            await cb(certs)
        }

        return Wendy_Agent_Services_V1_StartProvisioningResponse()
    }

    func isProvisioned(
        request: Wendy_Agent_Services_V1_IsProvisionedRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V1_IsProvisionedResponse {
        var response = Wendy_Agent_Services_V1_IsProvisionedResponse()
        if self.enrolled {
            var provisioned = Wendy_Agent_Services_V1_ProvisionedResponse()
            provisioned.cloudHost = self.cloudHost
            provisioned.organizationID = self.orgID
            provisioned.assetID = self.assetID
            response.provisioned = provisioned
        } else {
            response.notProvisioned = Wendy_Agent_Services_V1_NotProvisionedResponse()
        }
        return response
    }

    func unprovision(
        request: Wendy_Agent_Services_V1_UnprovisionRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V1_UnprovisionResponse {
        guard self.enrolled else {
            throw RPCError(code: .failedPrecondition, message: "agent is not provisioned")
        }
        // Cloud owns direct PKI unenrollment; this legacy reset must not erase it.
        guard self.principalURI.isEmpty else {
            throw RPCError(
                code: .failedPrecondition,
                message:
                    "direct PKI reset is disabled pending Cloud-authenticated authorization; keys retained"
            )
        }

        self.logger.info(
            "Unprovisioning device",
            metadata: ["org_id": "\(self.orgID)", "asset_id": "\(self.assetID)"]
        )

        do {
            try self.store.clear()
        } catch {
            self.logger.error(
                "Failed to delete provisioning state",
                metadata: ["error": "\(error)"]
            )
            throw RPCError(
                code: .internalError,
                message: "failed to delete provisioning state: \(error)"
            )
        }

        self.enrolled = false
        self.cloudHost = ""
        self.orgID = 0
        self.assetID = 0
        self.keyBacking = .softwarePEM("")
        self.seKey = nil
        self.certPEM = ""
        self.chainPEM = ""

        if let cb = self.onUnprovisioned {
            await cb()
        }

        return Wendy_Agent_Services_V1_UnprovisionResponse()
    }

    // MARK: - Direct PKI (served by the v2 StartACMEProvisioning RPC)

    /// Enrolls directly with pki-core's ACME frontend using a Cloud-minted EAB
    /// and returns the issued principal. Same check order and refusals as the
    /// Go agent, so the CLI sees the same answers from either agent.
    func startACMEProvisioning(
        cloudHost: String,
        config: ACMEEnrollmentConfig
    ) async throws -> String {
        do {
            try config.validate()
        } catch {
            throw RPCError(
                code: .invalidArgument,
                message: "invalid ACME enrollment config: \(error)"
            )
        }
        guard !cloudHost.isEmpty else {
            throw RPCError(code: .invalidArgument, message: "cloud host is required")
        }
        guard !self.enrolled else {
            throw RPCError(code: .failedPrecondition, message: "agent is already provisioned")
        }
        // The CLI's IsProvisioned preflight stops first; this backstop keeps any
        // other caller from spending an EAB on a chain this host cannot verify.
        if let reason = self.acmeEnrollmentUnavailableReason() {
            throw RPCError(code: .failedPrecondition, message: reason)
        }
        try self.beginProvisioning()
        defer { self.provisioningInFlight = false }
        let principal = try config.principalURI()

        // NEVER log the EAB key ID or HMAC key: they are a one-time bearer credential.
        self.logger.info(
            "Starting direct PKI provisioning",
            metadata: ["principal": "\(principal)", "cloud_host": "\(cloudHost)"]
        )

        // pki-core stamps the subject and SANs server-side; the CSR asserts only the key.
        let key = try self.generateDeviceKey(commonName: config.deviceID, identityURN: "")
        let issued: (certPEM: String, chainPEM: String)
        do {
            issued = try await self.acmeEnroll(
                config,
                self.configPath.appendingPathComponent("acme-account-key.pem"),
                key.csrDER,
                key.publicKey
            )
        } catch {
            throw RPCError(code: .internalError, message: "enrolling with PKI: \(error)")
        }
        guard !issued.chainPEM.isEmpty else {
            throw RPCError(
                code: .internalError,
                message: "PKI returned no certificate chain for device mTLS"
            )
        }
        // The mTLS server hands this chain to NIOSSL and verifies clients against
        // it with swift-certificates. A certificate either cannot load would lock
        // every client out after the switch, and a direct PKI identity cannot be
        // reset over RPC, so refuse before anything is persisted.
        let chainCount = (try? PEMDocument.parseMultiple(pemString: issued.chainPEM).count) ?? 0
        guard chainCount > 0,
            ClientCertAuthorizer.parseCertificates(pem: issued.chainPEM).count == chainCount,
            (try? NIOSSLCertificate.fromPEMBytes(Array(issued.chainPEM.utf8)).count) == chainCount
        else {
            throw RPCError(
                code: .internalError,
                message:
                    "PKI certificate chain cannot be loaded by this agent's mTLS stack; enrollment not persisted"
            )
        }

        do {
            try self.store.save(
                cloudHost: cloudHost,
                orgID: 0,
                assetID: 0,
                keyBacking: key.backing,
                certPEM: issued.certPEM,
                chainPEM: issued.chainPEM,
                principalURI: principal
            )
        } catch {
            self.logger.error(
                "Failed to persist provisioning state",
                metadata: ["error": "\(error)"]
            )
            throw RPCError(
                code: .internalError,
                message: "failed to save provisioning state: \(error)"
            )
        }

        self.enrolled = true
        self.cloudHost = cloudHost
        self.orgID = 0
        self.assetID = 0
        self.principalURI = principal
        self.keyBacking = key.backing
        self.seKey = key.seKey
        self.certPEM = issued.certPEM
        self.chainPEM = issued.chainPEM

        self.logger.info(
            "Direct PKI provisioning completed",
            metadata: ["principal": "\(principal)"]
        )

        if let cb = self.onProvisioned {
            await cb(
                ProvisioningCerts(
                    certPEM: self.certPEM,
                    chainPEM: self.chainPEM,
                    keyBacking: self.keyBacking,
                    seKey: self.seKey
                )
            )
        }
        return principal
    }

    // MARK: - Helpers

    /// pki-core issues device leaves under ML-DSA CAs, and the agent's mTLS
    /// verifies clients against that chain with swift-certificates, which on
    /// Darwin verifies ML-DSA only from macOS 26 (CryptoKit). Below that an
    /// enrolled Mac would reject every client, so it must not enroll at all.
    static func hostACMEEnrollmentUnavailableReason() -> String? {
        if #available(macOS 26.0, *) {
            return nil
        }
        let version = ProcessInfo.processInfo.operatingSystemVersion
        return
            "direct PKI enrollment needs macOS 26 or later to verify ML-DSA certificate chains; this Mac runs macOS \(version.majorVersion).\(version.minorVersion)"
    }

    private func beginProvisioning() throws {
        guard !self.provisioningInFlight else {
            throw RPCError(
                code: .failedPrecondition,
                message: "provisioning is already in progress"
            )
        }
        self.provisioningInFlight = true
    }

    private struct DeviceKey {
        var backing: ProvisioningStore.KeyBacking
        var seKey: SEPrivateKey?
        var csrPEM: String
        var csrDER: [UInt8]
        var publicKey: Certificate.PublicKey
    }

    /// Prefers the Secure Enclave when this Mac has one: the key is generated
    /// and signs entirely inside the enclave and never exists as extractable key
    /// material. Falls back to a software PEM key otherwise (older Macs, or
    /// hardware without an SE).
    ///
    /// Both tiers are P-256, never ML-DSA: the agent has no ML-DSA signer for
    /// the key and its CSR (swift-certificates verifies ML-DSA but cannot sign,
    /// the Secure Enclave and CryptoKit have no ML-DSA before macOS 26), and
    /// pki-core admits P-256 device keys for tiers A/B only as its explicit,
    /// audited fallback (`ecdsa_fallback`) until wendy-agent ships ML-DSA device
    /// keys (WDY-3620).
    private func generateDeviceKey(commonName: String, identityURN: String) throws -> DeviceKey {
        let backing: ProvisioningStore.KeyBacking
        let seKey: SEPrivateKey?
        let csrPEM: String
        let publicKey: Certificate.PublicKey
        if self.isSecureEnclaveAvailable() {
            let identity: SecureEnclaveIdentity
            do {
                identity = try SecureEnclaveIdentity.generate(store: KeychainStore())
            } catch {
                throw RPCError(
                    code: .internalError,
                    message: "failed to generate Secure Enclave key: \(error)"
                )
            }
            do {
                csrPEM = try DeviceIdentity.generateCSRPEM(
                    identity: identity,
                    commonName: commonName,
                    identityURN: identityURN
                )
            } catch {
                throw RPCError(code: .internalError, message: "failed to generate CSR: \(error)")
            }
            backing = .secureEnclave
            seKey = identity.nioCustomKey
            publicKey = identity.certificatePrivateKey.publicKey
        } else {
            let keyPEM: String
            do {
                keyPEM = try DeviceIdentity.generatePrivateKeyPEM()
                csrPEM = try DeviceIdentity.generateCSRPEM(
                    privateKeyPEM: keyPEM,
                    commonName: commonName,
                    identityURN: identityURN
                )
                publicKey = try Certificate.PrivateKey(pemEncoded: keyPEM).publicKey
            } catch {
                throw RPCError(
                    code: .internalError,
                    message: "failed to generate key pair or CSR: \(error)"
                )
            }
            backing = .softwarePEM(keyPEM)
            seKey = nil
        }
        let csrDER: [UInt8]
        do {
            csrDER = try PEMDocument(pemString: csrPEM).derBytes
        } catch {
            throw RPCError(code: .internalError, message: "failed to generate CSR: \(error)")
        }
        return DeviceKey(
            backing: backing,
            seKey: seKey,
            csrPEM: csrPEM,
            csrDER: csrDER,
            publicKey: publicKey
        )
    }
}
