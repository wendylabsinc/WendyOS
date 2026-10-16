import Crypto
import Foundation
import SwiftASN1
import Testing
import X509

@testable import WendyAgentCore

#if canImport(FoundationNetworking)
    import FoundationNetworking
#endif

private let testTenant = "11111111-1111-4111-8111-111111111111"

@Suite("ACMEEnrollment")
struct ACMEEnrollmentTests {
    private static let tenant = testTenant
    private static let config = ACMEEnrollmentConfig(
        directoryURL: "https://acme.test/\(tenant)/acme/directory",
        deviceID: "sim",
        eabKeyID: "eab-id",
        eabHMACKey: String(repeating: "ab", count: 32)
    )

    @Test("enrolls via EAB, newOrder and finalize, and returns leaf and chain")
    func enrolls() async throws {
        let (pki, device, dir) = try Self.setUp()
        defer { try? FileManager.default.removeItem(at: dir) }

        let issued = try await Self.enroll(pki, device, dir)

        let leaf = try Certificate(pemEncoded: issued.certPEM)
        #expect(leaf.publicKey == Certificate.PublicKey(device.publicKey))
        #expect(issued.chainPEM == pki.ca.pem + "\n")
        let accountKey = ACMEEnrollment.scopedAccountKeyPath(
            dir.appendingPathComponent("acme-account-key.pem"),
            Self.config
        )
        let mode =
            try FileManager.default.attributesOfItem(atPath: accountKey.path)[.posixPermissions]
            as? Int
        #expect(mode == 0o600)
    }

    @Test(
        "enrolls under a pki-core-shaped ML-DSA chain, which the agent's mTLS client check accepts",
        .enabled(if: TestPKI.mldsaAvailable, "ML-DSA verification needs macOS 26 on Darwin")
    )
    func enrollsUnderMLDSAChain() async throws {
        guard
            #available(macOS 26.0, iOS 26.0, watchOS 26.0, tvOS 26.0, macCatalyst 26.0,
            visionOS 26.0, *)
        else {
            return
        }
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-acme-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let pki = try FakePKI(
            tenant: Self.tenant,
            ca: TestPKI.makeMLDSAChain(),
            leafSignatureAlgorithm: .mldsa65
        )

        let issued = try await Self.enroll(pki, P256.Signing.PrivateKey(), dir)

        let chain = ClientCertAuthorizer.parseCertificates(pem: issued.chainPEM)
        #expect(chain.map(\.signatureAlgorithm) == [.mldsa87, .mldsa87])
        // The same verifier and tenant check the mTLS server runs on every client,
        // fed a leaf from this chain: an ML-DSA signature must verify end to end.
        let leaf = try Certificate(pemEncoded: issued.certPEM)
        #expect(leaf.signatureAlgorithm == .mldsa65)
        var serializer = DER.Serializer()
        try serializer.serialize(leaf)
        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [serializer.serializedBytes],
            trustRootsPEM: issued.chainPEM,
            deviceScope: ClientCertAuthorizer.scope(fromLeafPEM: issued.certPEM),
            mode: .strict
        )
        #expect(authorized)
    }

    @Test("a retry reuses the persisted account, so the single-use EAB is presented once")
    func retryReusesAccount() async throws {
        let (pki, device, dir) = try Self.setUp()
        defer { try? FileManager.default.removeItem(at: dir) }

        _ = try await Self.enroll(pki, device, dir)
        _ = try await Self.enroll(pki, device, dir)

        #expect(await pki.accountsCreated == 1)
    }

    @Test("falls back to GET when PKI serves the certificate GET-only")
    func certificateGETFallback() async throws {
        let (pki, device, dir) = try Self.setUp()
        defer { try? FileManager.default.removeItem(at: dir) }
        await pki.setCertificateGETOnly()

        _ = try await Self.enroll(pki, device, dir)
    }

    @Test("refuses a certificate naming another device")
    func wrongPrincipalRefused() async throws {
        let (pki, device, dir) = try Self.setUp()
        defer { try? FileManager.default.removeItem(at: dir) }
        await pki.setIssuedPrincipal("spiffe://wendy.sh/tenant/\(Self.tenant)/device/other")

        await #expect(throws: ACMEEnrollmentError.self) {
            _ = try await Self.enroll(pki, device, dir)
        }
    }

    @Test(
        "directory and device ID validation matches the Go agent",
        arguments: [
            ("https://acme.test/\(testTenant)/acme/directory", "fleet/box-01", true),
            ("http://127.0.0.1:14000/\(testTenant)/acme/directory", "sim", true),
            ("http://localhost/\(testTenant)/acme/directory", "sim", true),
            ("http://acme.test/\(testTenant)/acme/directory", "sim", false),
            ("https://acme.test/AAAAAAAA-1111-4111-8111-111111111111/acme/directory", "sim", false),
            ("https://acme.test/\(testTenant)/directory", "sim", false),
            ("https://acme.test/\(testTenant)/acme/directory?x=1", "sim", false),
            ("https://acme.test/\(testTenant)/acme/directory", "a/../b", false),
            ("https://acme.test/\(testTenant)/acme/directory", "a b", false),
        ]
    )
    func validation(directory: String, deviceID: String, valid: Bool) {
        var config = Self.config
        config.directoryURL = directory
        config.deviceID = deviceID
        #expect(((try? config.validate()) != nil) == valid)
    }

    @Test(
        "EAB key must be plain hex, as Go's hex decoder requires",
        arguments: ["+a+a", "-0-0", "abc", "zz"]
    )
    func signedHexRefused(key: String) {
        var config = Self.config
        config.eabHMACKey = key
        #expect(throws: ACMEEnrollmentError.self) { try config.validate() }
    }

    // MARK: - Helpers

    private static func setUp() throws -> (FakePKI, P256.Signing.PrivateKey, URL) {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-acme-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return (try FakePKI(tenant: Self.tenant), P256.Signing.PrivateKey(), dir)
    }

    private static func enroll(
        _ pki: FakePKI,
        _ device: P256.Signing.PrivateKey,
        _ dir: URL
    ) async throws -> (certPEM: String, chainPEM: String) {
        let csr = try CertificateSigningRequest(
            version: .v1,
            subject: DistinguishedName { CommonName("sim") },
            privateKey: Certificate.PrivateKey(device),
            attributes: CertificateSigningRequest.Attributes(),
            signatureAlgorithm: .ecdsaWithSHA256
        )
        var serializer = DER.Serializer()
        try serializer.serialize(csr)
        return try await ACMEEnrollment.enroll(
            Self.config,
            accountKeyPath: dir.appendingPathComponent("acme-account-key.pem"),
            csrDER: serializer.serializedBytes,
            devicePublicKey: Certificate.PublicKey(device.publicKey),
            transport: { try await pki.handle($0) }
        )
    }
}

/// An in-memory pki-core ACME frontend. It checks what the real one checks on
/// the client's side of the wire (fresh nonces, ES256 JWS over the request URL,
/// the hex EAB's HS256 MAC over the account JWK, the permanent-identifier
/// order) and issues from a test CA, stamping the identity itself.
private actor FakePKI {
    let ca: TestPKI.CA
    private let leafSignatureAlgorithm: Certificate.SignatureAlgorithm
    private let base: String
    private let tenant: String
    private var nonces: Set<String> = []
    private var accounts: [String: P256.Signing.PublicKey] = [:]
    private var leafPEM = ""
    private var certificateGETOnly = false
    private var issuedPrincipal: String
    private(set) var accountsCreated = 0

    init(
        tenant: String,
        ca: TestPKI.CA? = nil,
        leafSignatureAlgorithm: Certificate.SignatureAlgorithm = .ecdsaWithSHA256
    ) throws {
        self.ca = try ca ?? TestPKI.makeCA()
        self.leafSignatureAlgorithm = leafSignatureAlgorithm
        self.tenant = tenant
        self.base = "https://acme.test/\(tenant)/acme/"
        self.issuedPrincipal = "spiffe://wendy.sh/tenant/\(tenant)/device/sim"
    }

    func setCertificateGETOnly() { self.certificateGETOnly = true }
    func setIssuedPrincipal(_ principal: String) { self.issuedPrincipal = principal }

    func handle(_ request: URLRequest) throws -> (Data, HTTPURLResponse) {
        let url = request.url!.absoluteString
        switch (request.httpMethod ?? "GET", url) {
        case ("GET", self.base + "directory"):
            return try self.reply(
                200,
                json: [
                    "newNonce": self.base + "new-nonce", "newAccount": self.base + "new-account",
                    "newOrder": self.base + "new-order",
                ]
            )
        case ("HEAD", self.base + "new-nonce"):
            return self.reply(200, Data())
        case ("GET", self.base + "cert/1"):
            return self.reply(200, Data((self.leafPEM + self.ca.pem).utf8))
        case ("POST", _):
            return try self.post(url, request.httpBody ?? Data())
        default:
            return self.reply(404, Data("404 page not found".utf8))
        }
    }

    private func post(_ url: String, _ body: Data) throws -> (Data, HTTPURLResponse) {
        let jws = try JSONDecoder().decode(ACMEEnrollment.JWS.self, from: body)
        let header = try JSONDecoder().decode(
            ACMEEnrollment.Header.self,
            from: Self.unb64(jws.protected)
        )
        try #require(self.nonces.remove(header.nonce) != nil, "stale or unknown nonce")
        try #require(header.url == url && header.alg == "ES256")
        let key: P256.Signing.PublicKey
        if let jwk = header.jwk {
            try #require(header.kid == nil && url == self.base + "new-account")
            key = try P256.Signing.PublicKey(
                rawRepresentation: Self.unb64(jwk.x) + Self.unb64(jwk.y)
            )
        } else {
            key = try #require(self.accounts[header.kid ?? ""])
        }
        let signature = try P256.Signing.ECDSASignature(
            rawRepresentation: Self.unb64(jws.signature)
        )
        try #require(
            key.isValidSignature(signature, for: Data("\(jws.protected).\(jws.payload)".utf8))
        )
        let payload = try Self.unb64(jws.payload)

        switch url {
        case self.base + "new-account":
            let kid = self.base + "acct/" + key.rawRepresentation.base64EncodedString()
            if self.accounts[kid] != nil {
                return self.reply(200, Data("{}".utf8), location: kid)
            }
            let account = try JSONDecoder().decode(ACMEEnrollment.NewAccount.self, from: payload)
            let eab = account.externalAccountBinding
            let eabHeader = try JSONDecoder().decode(
                ACMEEnrollment.EABHeader.self,
                from: Self.unb64(eab.protected)
            )
            try #require(
                eabHeader.alg == "HS256" && eabHeader.kid == "eab-id" && eabHeader.url == url
            )
            let mac = HMAC<SHA256>.authenticationCode(
                for: Data("\(eab.protected).\(eab.payload)".utf8),
                using: SymmetricKey(
                    data: ACMEEnrollment.hexDecode(String(repeating: "ab", count: 32))!
                )
            )
            let eabSignature = try Self.unb64(eab.signature)
            try #require(Data(mac) == eabSignature, "EAB MAC")
            let boundKey = try JSONDecoder().decode(
                ACMEEnrollment.JWK.self,
                from: Self.unb64(eab.payload)
            )
            try #require(boundKey == header.jwk, "EAB must bind the account key")
            self.accounts[kid] = key
            self.accountsCreated += 1
            return self.reply(201, Data("{}".utf8), location: kid)
        case self.base + "new-order":
            let order = try JSONDecoder().decode(ACMEEnrollment.NewOrder.self, from: payload)
            try #require(order.identifiers.map(\.type) == ["permanent-identifier"])
            try #require(order.identifiers.map(\.value) == ["sim"])
            return try self.reply(
                201,
                json: ["status": "ready", "finalize": self.base + "finalize/1"],
                location: self.base + "order/1"
            )
        case self.base + "finalize/1":
            let csr = try JSONDecoder().decode([String: String].self, from: payload)
            let request = try CertificateSigningRequest(
                derEncoded: Array(Self.unb64(csr["csr"] ?? ""))
            )
            let leaf = try Certificate(
                version: .v3,
                serialNumber: Certificate.SerialNumber(),
                publicKey: request.publicKey,
                notValidBefore: Date().addingTimeInterval(-60),
                notValidAfter: Date().addingTimeInterval(3600),
                issuer: self.ca.certificate.subject,
                subject: try DistinguishedName { CommonName(self.issuedPrincipal) },
                signatureAlgorithm: self.leafSignatureAlgorithm,
                extensions: try Certificate.Extensions {
                    Critical(KeyUsage(digitalSignature: true))
                    try ExtendedKeyUsage([.clientAuth, .serverAuth])
                    SubjectAlternativeNames([.uniformResourceIdentifier(self.issuedPrincipal)])
                },
                issuerPrivateKey: self.ca.privateKey
            )
            self.leafPEM = try leaf.serializeAsPEM().pemString + "\n"
            return try self.reply(
                200,
                json: [
                    "status": "valid", "finalize": self.base + "finalize/1",
                    "certificate": self.base + "cert/1",
                ]
            )
        case self.base + "cert/1":
            try #require(payload.isEmpty, "certificate fetch must be POST-as-GET")
            if self.certificateGETOnly {
                return self.reply(405, Data())
            }
            return self.reply(200, Data((self.leafPEM + self.ca.pem).utf8))
        default:
            return self.reply(404, Data("404 page not found".utf8))
        }
    }

    private func reply(
        _ status: Int,
        json: [String: String],
        location: String? = nil
    ) throws -> (Data, HTTPURLResponse) {
        self.reply(status, try JSONSerialization.data(withJSONObject: json), location: location)
    }

    private func reply(
        _ status: Int,
        _ body: Data,
        location: String? = nil
    ) -> (Data, HTTPURLResponse) {
        let nonce = UUID().uuidString
        self.nonces.insert(nonce)
        var headers = ["Replay-Nonce": nonce]
        headers["Location"] = location
        return (
            body,
            HTTPURLResponse(
                url: URL(string: self.base)!,
                statusCode: status,
                httpVersion: nil,
                headerFields: headers
            )!
        )
    }

    private static func unb64(_ string: String) throws -> Data {
        var s = string.replacingOccurrences(of: "-", with: "+").replacingOccurrences(
            of: "_",
            with: "/"
        )
        s += String(repeating: "=", count: (4 - s.count % 4) % 4)
        return try #require(Data(base64Encoded: s))
    }
}
