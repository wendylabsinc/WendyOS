import Foundation
import SwiftASN1
import X509

/// Decides whether an incoming mTLS client certificate chain may talk to this
/// device's main gRPC server. This is the security core of the agent's mTLS
/// path: it (1) verifies the peer-presented chain against the device's own CA
/// trust roots — the same chain the cloud issued the device — and then (2)
/// enforces org-equality, rejecting a client whose organization differs from
/// this device's.
///
/// This runs INSIDE NIOSSL's custom verification callback, which fully replaces
/// BoringSSL's built-in verification (see `NIOSSLCustomVerificationCallbackWithMetadata`:
/// "Setting this callback will override _all_ verification logic that BoringSSL
/// provides"). Therefore this function is solely responsible for building a
/// verified path to a trusted root; it never assumes the peer-presented
/// certificates are valid. It fails closed: any parse or verification failure
/// returns `false`.
///
/// "Organization" here means the tenant a certificate belongs to: the tenant
/// SPIFFE principal pki-core stamps, or the legacy `urn:wendy:org` on an old
/// chain. See `OrgIdentity.Scope`.
enum ClientCertAuthorizer {
    /// How the org-equality gate treats the connecting client certificate,
    /// mirroring the Go agent's `interceptor.OrgMode`.
    enum OrgEnforcementMode: Sendable, Equatable {
        /// No org check: any client whose chain verifies is accepted.
        case off
        /// Enforce tenant-equality for certs that carry an identity, but allow
        /// legacy certs that carry none — and certs whose tenant this device has
        /// no way to compare with its own, which is the state a fleet passes
        /// through while its certificates rotate onto pki-core chains. This is
        /// the default and lets today's `wendy` CLI user cert
        /// (`wendy/user/<uid>`, no claim) connect meanwhile.
        case grace
        /// Enforce tenant-equality AND require every client cert to carry a
        /// tenant this device can compare with its own; a legacy no-identity
        /// cert, and one naming an incomparable tenant, are both rejected.
        case strict

        var name: String {
            switch self {
            case .off: return "off"
            case .grace: return "grace"
            case .strict: return "strict"
            }
        }

        /// Maps a `WENDY_MTLS_ORG_ENFORCEMENT` value to a mode. An empty/absent
        /// value yields `(.grace, true)`. The values `off`, `grace`, `strict`
        /// (case-insensitive, trimmed) yield the matching mode and `true`. Any
        /// other value yields `(.grace, false)` so the caller can warn and fall
        /// back to grace.
        static func parse(_ raw: String?) -> (mode: OrgEnforcementMode, recognized: Bool) {
            switch (raw ?? "").trimmingCharacters(in: .whitespaces).lowercased() {
            case "": return (.grace, true)
            case "off": return (.off, true)
            case "grace": return (.grace, true)
            case "strict": return (.strict, true)
            default: return (.grace, false)
            }
        }
    }

    /// - Parameters:
    ///   - peerCertificatesDER: The peer-presented certificate chain, DER-encoded,
    ///     leaf first (the order NIOSSL delivers them in).
    ///   - trustRootsPEM: The device's CA chain (PEM), used as the trust anchors.
    ///   - deviceScope: This device's own tenant, or `nil` if it could not be
    ///     determined from the device's own certificate. When `nil` (and the mode
    ///     is not `.off`), every client is rejected (fail closed): tenant-equality
    ///     is the sole cross-tenant barrier and is never silently dropped.
    ///   - mode: The org-enforcement mode (default `.grace`). See
    ///     ``OrgEnforcementMode``.
    /// - Returns: `true` iff the chain verifies to a trusted root AND the tenant
    ///   policy for `mode` is satisfied.
    static func isAuthorized(
        peerCertificatesDER: [[UInt8]],
        trustRootsPEM: String,
        deviceScope: OrgIdentity.Scope?,
        mode: OrgEnforcementMode = .grace
    ) async -> Bool {
        // Parse trust anchors from the device CA chain. Without any anchors we
        // cannot verify a path, so fail closed.
        let roots = Self.parseCertificates(pem: trustRootsPEM)
        guard !roots.isEmpty else { return false }

        // Parse the peer-presented chain: leaf first, remainder are intermediates.
        let peerCerts = peerCertificatesDER.compactMap { try? Certificate(derEncoded: $0) }
        guard let leaf = peerCerts.first else { return false }
        let intermediates = Array(peerCerts.dropFirst())

        // Mandatory: build a verified path from the leaf to a trusted root.
        var verifier = Verifier(rootCertificates: CertificateStore(roots)) {
            RFC5280Policy()
        }
        let result = await verifier.validate(
            leaf: leaf,
            intermediates: CertificateStore(intermediates)
        )
        guard case .validCertificate = result else { return false }

        // `.off` opts out of tenant enforcement entirely: any client whose chain
        // verifies to a trusted root is accepted.
        if mode == .off { return true }

        // Additional layer: tenant-equality. Because the PKI shares CA roots
        // across organizations, chain verification alone would let a validly
        // provisioned entity from another tenant connect — tenant-equality is
        // the sole cross-tenant barrier. If the device's own tenant is unknown
        // we reject every client (in grace and strict) rather than silently
        // dropping that barrier: a device with an unparseable cert becomes
        // unreachable over mTLS, the safe failure mode. Re-provision to recover.
        guard let deviceScope else { return false }

        // Extract the client's claim. A present-but-malformed claim (thrown
        // error) is anomalous and rejected under every mode.
        let clientScope: OrgIdentity.Scope?
        do {
            clientScope = try OrgIdentity.scope(fromLeaf: leaf)
        } catch {
            return false
        }

        guard let clientScope else {
            // A legacy cert carrying no identity (e.g. the CLI's user cert).
            // Allowed under grace, rejected under strict.
            return mode == .grace
        }

        if clientScope.matches(deviceScope) { return true }
        // A different tenant, said in terms this device can check, is refused
        // under grace as well as strict: grace forgives an identity it cannot
        // read, never one it can read and that says someone else.
        if clientScope.comparable(with: deviceScope) { return false }
        // Otherwise the client named a tenant with no shared vocabulary — a
        // SPIFFE-only caller reaching a device still on an old chain, or the
        // reverse. Unprovable rather than wrong, and exactly the rotation
        // window grace exists for.
        return mode == .grace
    }

    /// This device's own tenant, read from the leaf certificate of the given
    /// PEM, or `nil` if it can't be parsed. Used when building the mTLS server.
    static func scope(fromLeafPEM pem: String) -> OrgIdentity.Scope? {
        guard let leaf = Self.parseCertificates(pem: pem).first else { return nil }
        return (try? OrgIdentity.scope(fromLeaf: leaf)) ?? nil
    }

    /// Parses zero or more PEM `CERTIFICATE` blocks into certificates, ignoring
    /// any block that isn't a certificate or that fails to parse.
    static func parseCertificates(pem: String) -> [Certificate] {
        guard let documents = try? PEMDocument.parseMultiple(pemString: pem) else {
            return []
        }
        return documents.compactMap { try? Certificate(pemDocument: $0) }
    }
}
