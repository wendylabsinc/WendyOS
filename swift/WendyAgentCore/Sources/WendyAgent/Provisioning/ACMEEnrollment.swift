import Crypto
import Foundation
import SwiftASN1
import X509

#if canImport(FoundationNetworking)
    import FoundationNetworking
#endif

/// Enrollment material Cloud mints for one direct PKI enrollment. Mirrors the
/// Go agent's `acmeenroll.Config`, including its validation messages.
struct ACMEEnrollmentConfig: Sendable {
    /// The tenant's ACME directory, `https://<host>/<tenant-uuid>/acme/directory`.
    var directoryURL: String
    /// Stamped into the issued SPIFFE identity server-side; must equal the
    /// device ID bound to the EAB row.
    var deviceID: String
    var eabKeyID: String
    /// HEX-encoded, not base64url: pki-core hands it out that way.
    var eabHMACKey: String

    func validate() throws {
        let missing = [
            ("directoryURL", self.directoryURL), ("deviceID", self.deviceID),
            ("eabKeyID", self.eabKeyID), ("eabHMACKey", self.eabHMACKey),
        ].filter { $0.1.isEmpty }.map(\.0)
        guard missing.isEmpty else {
            throw ACMEEnrollmentError(
                "acme enrollment config is missing \(missing.joined(separator: ", "))"
            )
        }
        _ = try self.principalURI()
        guard ACMEEnrollment.hexDecode(self.eabHMACKey) != nil else {
            throw ACMEEnrollmentError("EAB HMAC key must be hex-encoded")
        }
    }

    /// The device identity this tenant's ACME endpoint must issue. Validated
    /// before the single-use EAB is sent anywhere.
    func principalURI() throws -> String {
        guard let url = URLComponents(string: self.directoryURL), let host = url.host,
            !host.isEmpty, url.user == nil, url.password == nil, url.query == nil,
            url.fragment == nil
        else {
            throw ACMEEnrollmentError("invalid ACME directory URL")
        }
        guard url.scheme == "https" || (url.scheme == "http" && Self.isLoopback(host)) else {
            throw ACMEEnrollmentError(
                "ACME directory must use HTTPS (HTTP is allowed only for loopback development servers)"
            )
        }
        let parts = url.path.dropFirst().split(separator: "/", omittingEmptySubsequences: false)
        guard parts.count == 3, parts[1] == "acme", parts[2] == "directory" else {
            throw ACMEEnrollmentError(
                "ACME directory URL must have the path /<tenant-uuid>/acme/directory"
            )
        }
        let tenant = String(parts[0])
        guard let uuid = UUID(uuidString: tenant), uuid.uuidString.lowercased() == tenant else {
            throw ACMEEnrollmentError("ACME directory URL must contain a canonical tenant UUID")
        }
        // Match PKI's device-name contract, including names such as fleet/box-01.
        for segment in self.deviceID.split(separator: "/", omittingEmptySubsequences: false) {
            guard (1...64).contains(segment.count), segment != ".", segment != ".." else {
                throw ACMEEnrollmentError(
                    "invalid ACME device ID: each path segment must contain 1–64 letters, digits, dots, underscores or hyphens, excluding . and .."
                )
            }
            let allowed = { (c: Unicode.Scalar) in
                ("a"..."z").contains(c) || ("A"..."Z").contains(c) || ("0"..."9").contains(c)
                    || ".-_".unicodeScalars.contains(c)
            }
            guard segment.unicodeScalars.allSatisfy(allowed) else {
                throw ACMEEnrollmentError(
                    "invalid ACME device ID: use letters, digits, dots, underscores, hyphens and path separators"
                )
            }
        }
        return "spiffe://wendy.sh/tenant/\(tenant)/device/\(self.deviceID)"
    }

    private static func isLoopback(_ host: String) -> Bool {
        let host = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        if host == "localhost" || host == "::1" { return true }
        let octets = host.split(separator: ".", omittingEmptySubsequences: false).compactMap {
            UInt8($0)
        }
        return octets.count == 4 && octets[0] == 127
    }
}

struct ACMEEnrollmentError: Error, CustomStringConvertible {
    var description: String
    /// The HTTP status of an ACME problem response, when there was one.
    var status: Int?

    init(_ description: String, status: Int? = nil) {
        self.description = description
        self.status = status
    }
}

/// A device certificate from pki-core's ACME frontend via an External Account
/// Binding. The Swift twin of the Go agent's `acmeenroll.Enroll`; pki-core's
/// deviations from a public CA are described there (hex EAB key, the
/// `permanent-identifier` identifier, identity stamped server-side, a
/// certificate URL that may answer GET only).
enum ACMEEnrollment {
    typealias Transport = @Sendable (URLRequest) async throws -> (Data, HTTPURLResponse)

    /// Registers the ACME account, orders a certificate for `config.deviceID`
    /// with `csrDER`, and returns the issued leaf and the chain below it (PEM).
    ///
    /// The account key is persisted under `accountKeyPath`'s `.d` directory,
    /// scoped to the directory URL and device ID, BEFORE the single-use EAB is
    /// presented, so a disk error cannot burn the credential and a retry
    /// re-registers the same account.
    static func enroll(
        _ config: ACMEEnrollmentConfig,
        accountKeyPath: URL,
        csrDER: [UInt8],
        devicePublicKey: Certificate.PublicKey,
        transport: @escaping Transport = Self.liveTransport
    ) async throws -> (certPEM: String, chainPEM: String) {
        try config.validate()
        let expected = try config.principalURI()
        let hmacKey = Self.hexDecode(config.eabHMACKey) ?? []
        let accountKey = try Self.loadOrCreateAccountKey(
            Self.scopedAccountKeyPath(accountKeyPath, config)
        )

        var session = Session(transport: transport, key: accountKey)
        let directory: Directory = try await session.get(config.directoryURL)

        do {
            let eabProtected = try Self.b64url(
                Self.json(EABHeader(alg: "HS256", kid: config.eabKeyID, url: directory.newAccount))
            )
            let eabPayload = try Self.b64url(Self.json(session.jwk))
            let mac = HMAC<SHA256>.authenticationCode(
                for: Data("\(eabProtected).\(eabPayload)".utf8),
                using: SymmetricKey(data: hmacKey)
            )
            let account = NewAccount(
                termsOfServiceAgreed: true,
                externalAccountBinding: JWS(
                    protected: eabProtected,
                    payload: eabPayload,
                    signature: Self.b64url(Data(mac))
                )
            )
            // 201 creates the account, 200 returns the existing one for this key.
            let (_, response) = try await session.post(
                directory.newAccount,
                payload: Self.json(account)
            )
            guard let kid = response.value(forHTTPHeaderField: "Location"), !kid.isEmpty else {
                throw ACMEEnrollmentError("ACME server returned no account URL")
            }
            session.kid = kid
        } catch {
            throw ACMEEnrollmentError("registering ACME account: \(error)")
        }

        let newOrder = NewOrder(identifiers: [
            .init(type: "permanent-identifier", value: config.deviceID)
        ])
        let (orderData, orderResponse): (Data, HTTPURLResponse)
        do {
            (orderData, orderResponse) = try await session.post(
                directory.newOrder,
                payload: Self.json(newOrder)
            )
        } catch {
            throw ACMEEnrollmentError(
                "creating ACME order for device \"\(config.deviceID)\": \(error)"
            )
        }
        let orderURL = orderResponse.value(forHTTPHeaderField: "Location")
        var order = try JSONDecoder().decode(Order.self, from: orderData)
        guard order.status == "ready" else {
            throw ACMEEnrollmentError(
                "ACME order is \"\(order.status)\", want \"ready\": this device's profile requires an attestation challenge, which is not implemented"
            )
        }

        do {
            let (data, _) = try await session.post(
                order.finalize,
                payload: Self.json(["csr": Self.b64url(Data(csrDER))])
            )
            order = try JSONDecoder().decode(Order.self, from: data)
            // ponytail: fixed 1 s poll for up to a minute; honour Retry-After if issuance gets slower.
            var polls = 0
            while order.status == "processing" || order.status == "ready", let orderURL, polls < 60
            {
                try await Task.sleep(for: .seconds(1))
                order = try JSONDecoder().decode(
                    Order.self,
                    from: try await session.post(orderURL, payload: nil).0
                )
                polls += 1
            }
        } catch {
            throw ACMEEnrollmentError("finalizing ACME order: \(error)")
        }
        guard order.status == "valid", let certURL = order.certificate else {
            throw ACMEEnrollmentError(
                "finalizing ACME order: order is \"\(order.status)\", want \"valid\""
            )
        }

        let ders: [[UInt8]]
        do {
            let pem: Data
            do {
                pem = try await session.post(certURL, payload: nil).0
            } catch let error as ACMEEnrollmentError
                where error.status == 404 || error.status == 405
            {
                // PKI may expose the certificate resource as GET only.
                pem = try await Self.fetchPKICertificate(
                    config.directoryURL,
                    certURL,
                    transport: transport
                )
            }
            ders = try Self.parseChain(pem)
        } catch {
            throw ACMEEnrollmentError("downloading issued ACME certificate: \(error)")
        }

        let leaf = try Certificate(derEncoded: ders[0])
        guard leaf.publicKey == devicePublicKey else {
            throw ACMEEnrollmentError("ACME certificate does not match the device key")
        }
        let principals = (try? leaf.extensions.subjectAlternativeNames)?.compactMap {
            name -> String? in
            guard case .uniformResourceIdentifier(let uri) = name,
                uri.hasPrefix("spiffe://wendy.sh/tenant/")
            else { return nil }
            return uri
        }
        guard principals == [expected] else {
            throw ACMEEnrollmentError(
                "ACME certificate does not identify the expected device \(expected)"
            )
        }
        // Enrollment switches the agent to mTLS. Reject a profile that would lock
        // out inbound clients or outbound authentication before it is persisted.
        let usages = (try? leaf.extensions.extendedKeyUsage).map(Array.init) ?? []
        guard usages.contains(.clientAuth), usages.contains(.serverAuth) else {
            throw ACMEEnrollmentError(
                "ACME device certificate must explicitly permit clientAuth and serverAuth; correct the PKI device profile and reissue the certificate"
            )
        }
        return (Self.encode(ders.prefix(1)), Self.encode(ders.dropFirst()))
    }

    static let liveTransport: Transport = { request in
        let (data, response) = try await noRedirectSession.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw ACMEEnrollmentError("ACME server returned a non-HTTP response")
        }
        return (data, http)
    }

    // MARK: - JWS session

    /// Signs requests with the account key: `jwk` until the account exists,
    /// then `kid`. Carries the Replay-Nonce between requests.
    private struct Session {
        let transport: Transport
        let key: P256.Signing.PrivateKey
        var kid: String?
        private var nonce: String?
        private var newNonceURL: String?

        init(transport: @escaping Transport, key: P256.Signing.PrivateKey) {
            self.transport = transport
            self.key = key
        }

        var jwk: JWK {
            let raw = self.key.publicKey.rawRepresentation
            return JWK(
                crv: "P-256",
                kty: "EC",
                x: ACMEEnrollment.b64url(raw.prefix(32)),
                y: ACMEEnrollment.b64url(raw.suffix(32))
            )
        }

        mutating func get(_ url: String) async throws -> Directory {
            let (data, response) = try await self.send(Self.request(url, method: "GET"))
            try ACMEEnrollment.check(data, response)
            let directory = try JSONDecoder().decode(Directory.self, from: data)
            self.newNonceURL = directory.newNonce
            return directory
        }

        /// A JWS POST; `payload == nil` is POST-as-GET. A badNonce is retried
        /// once with the nonce the refusal carried, as RFC 8555 §6.5 asks.
        mutating func post(_ url: String, payload: Data?) async throws -> (Data, HTTPURLResponse) {
            for attempt in 0..<2 {
                let nonce = try await self.takeNonce()
                let header = try ACMEEnrollment.b64url(
                    ACMEEnrollment.json(
                        Header(
                            alg: "ES256",
                            jwk: self.kid == nil ? self.jwk : nil,
                            kid: self.kid,
                            nonce: nonce,
                            url: url
                        )
                    )
                )
                let body = payload.map(ACMEEnrollment.b64url) ?? ""
                let signature = try self.key.signature(for: Data("\(header).\(body)".utf8))
                var request = try Self.request(url, method: "POST")
                request.setValue("application/jose+json", forHTTPHeaderField: "Content-Type")
                request.httpBody = try ACMEEnrollment.json(
                    JWS(
                        protected: header,
                        payload: body,
                        signature: ACMEEnrollment.b64url(signature.rawRepresentation)
                    )
                )
                let (data, response) = try await self.send(request)
                do {
                    try ACMEEnrollment.check(data, response)
                    return (data, response)
                } catch let error as ACMEEnrollmentError
                    where attempt == 0
                    && error.description.contains("urn:ietf:params:acme:error:badNonce")
                {
                    continue
                }
            }
            throw ACMEEnrollmentError("ACME server kept rejecting nonces")
        }

        private mutating func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
            let (data, response) = try await self.transport(request)
            self.nonce = response.value(forHTTPHeaderField: "Replay-Nonce")
            return (data, response)
        }

        private mutating func takeNonce() async throws -> String {
            if let nonce = self.nonce {
                self.nonce = nil
                return nonce
            }
            guard let url = self.newNonceURL else {
                throw ACMEEnrollmentError("ACME directory has no newNonce")
            }
            _ = try await self.send(Self.request(url, method: "HEAD"))
            guard let nonce = self.nonce else {
                throw ACMEEnrollmentError("ACME server returned no nonce")
            }
            self.nonce = nil
            return nonce
        }

        static func request(_ url: String, method: String) throws -> URLRequest {
            guard let parsed = URL(string: url) else {
                throw ACMEEnrollmentError("invalid ACME URL \(url)")
            }
            var request = URLRequest(url: parsed)
            request.httpMethod = method
            return request
        }
    }

    // MARK: - Helpers

    /// PKI's documented GET certificate endpoint, restricted to the selected
    /// tenant's certificate path on the directory's origin.
    private static func fetchPKICertificate(
        _ directory: String,
        _ certURL: String,
        transport: Transport
    ) async throws -> Data {
        guard let dir = URLComponents(string: directory), let cert = URLComponents(string: certURL)
        else {
            throw ACMEEnrollmentError("invalid ACME directory URL")
        }
        let prefix = String(dir.percentEncodedPath.dropLast("/directory".count)) + "/cert/"
        guard cert.scheme == dir.scheme, cert.host == dir.host, cert.port == dir.port,
            cert.user == nil, cert.password == nil, cert.query == nil, cert.fragment == nil,
            cert.percentEncodedPath.hasPrefix(prefix), cert.percentEncodedPath.count > prefix.count
        else {
            throw ACMEEnrollmentError(
                "PKI certificate URL is outside the selected tenant's certificate endpoint"
            )
        }
        let (data, response) = try await transport(Session.request(certURL, method: "GET"))
        guard response.statusCode == 200 else {
            throw ACMEEnrollmentError(
                "PKI certificate download returned HTTP \(response.statusCode)"
            )
        }
        return data
    }

    private static func check(_ data: Data, _ response: HTTPURLResponse) throws {
        guard response.statusCode >= 400 else { return }
        let problem = try? JSONDecoder().decode(Problem.self, from: data)
        throw ACMEEnrollmentError(
            "HTTP \(response.statusCode): \(problem?.type ?? "") \(problem?.detail ?? String(decoding: data.prefix(512), as: UTF8.self))",
            status: response.statusCode
        )
    }

    private static func parseChain(_ data: Data) throws -> [[UInt8]] {
        let maxChainBytes = 256 * 1024
        guard data.count <= maxChainBytes else {
            throw ACMEEnrollmentError("PKI certificate chain exceeds 256 KiB")
        }
        let documents = try PEMDocument.parseMultiple(
            pemString: String(decoding: data, as: UTF8.self)
        )
        guard !documents.isEmpty, documents.count <= 10,
            documents.allSatisfy({ $0.discriminator == "CERTIFICATE" })
        else {
            throw ACMEEnrollmentError("invalid PKI certificate chain")
        }
        return documents.map(\.derBytes)
    }

    private static func encode(_ ders: some Sequence<[UInt8]>) -> String {
        ders.map { PEMDocument(type: "CERTIFICATE", derBytes: $0).pemString + "\n" }.joined()
    }

    /// `<base>.d/<sha256(directory \0 deviceID)>.pem`, the Go agent's layout: a
    /// device renamed after a failed enrollment never reuses another name's account.
    static func scopedAccountKeyPath(_ base: URL, _ config: ACMEEnrollmentConfig) -> URL {
        let scope = SHA256.hash(data: Data("\(config.directoryURL)\u{0}\(config.deviceID)".utf8))
        return base.deletingLastPathComponent()
            .appendingPathComponent(base.lastPathComponent + ".d", isDirectory: true)
            .appendingPathComponent(
                scope.map { ($0 < 16 ? "0" : "") + String($0, radix: 16) }.joined() + ".pem"
            )
    }

    /// The persisted P-256 account key (ES256 is what pki-core's outer-JWS
    /// allow-list accepts), created 0600 on first use.
    private static func loadOrCreateAccountKey(_ path: URL) throws -> P256.Signing.PrivateKey {
        if let pem = try? String(contentsOf: path, encoding: .utf8) {
            return try P256.Signing.PrivateKey(pemRepresentation: pem)
        }
        let key = P256.Signing.PrivateKey()
        let manager = FileManager.default
        let dir = path.deletingLastPathComponent()
        try manager.createDirectory(
            at: dir,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        let tmp = dir.appendingPathComponent(".\(UUID().uuidString).tmp")
        guard
            manager.createFile(
                atPath: tmp.path,
                contents: nil,
                attributes: [.posixPermissions: 0o600]
            )
        else {
            throw ACMEEnrollmentError("storing ACME account key \(path.path) failed")
        }
        do {
            // Durable before the rename: the EAB this key is about to bind is single-use.
            let handle = try FileHandle(forWritingTo: tmp)
            defer { try? handle.close() }
            try handle.write(contentsOf: Data(key.pemRepresentation.utf8))
            try handle.synchronize()
            try manager.moveItem(at: tmp, to: path)
        } catch {
            try? manager.removeItem(at: tmp)
            throw ACMEEnrollmentError("storing ACME account key \(path.path): \(error)")
        }
        return key
    }

    static func hexDecode(_ hex: String) -> [UInt8]? {
        let digits = Array(hex.utf8)
        // UInt8(_:radix:) alone would accept a "+"/"-" sign that Go's hex decoder refuses.
        guard digits.count % 2 == 0, hex.allSatisfy(\.isHexDigit) else { return nil }
        var bytes: [UInt8] = []
        bytes.reserveCapacity(digits.count / 2)
        for index in stride(from: 0, to: digits.count, by: 2) {
            guard
                let byte = UInt8(
                    String(decoding: digits[index...index + 1], as: UTF8.self),
                    radix: 16
                )
            else { return nil }
            bytes.append(byte)
        }
        return bytes
    }

    static func b64url(_ data: Data) -> String {
        data.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    private static func json(_ value: some Encodable) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return try encoder.encode(value)
    }

    private static let noRedirectSession = URLSession(
        configuration: .ephemeral,
        delegate: NoRedirects(),
        delegateQueue: nil
    )

    private final class NoRedirects: NSObject, URLSessionTaskDelegate, Sendable {
        func urlSession(
            _ session: URLSession,
            task: URLSessionTask,
            willPerformHTTPRedirection response: HTTPURLResponse,
            newRequest request: URLRequest
        ) async -> URLRequest? {
            nil
        }
    }

    // MARK: - Wire types (RFC 8555)

    struct JWK: Codable, Equatable {
        var crv: String
        var kty: String
        var x: String
        var y: String
    }

    struct Header: Codable {
        var alg: String
        var jwk: JWK?
        var kid: String?
        var nonce: String
        var url: String
    }

    struct EABHeader: Codable {
        var alg: String
        var kid: String
        var url: String
    }

    struct JWS: Codable {
        var protected: String
        var payload: String
        var signature: String
    }

    struct NewAccount: Codable {
        var termsOfServiceAgreed: Bool
        var externalAccountBinding: JWS
    }

    struct NewOrder: Codable {
        struct Identifier: Codable {
            var type: String
            var value: String
        }
        var identifiers: [Identifier]
    }

    struct Directory: Codable {
        var newNonce: String
        var newAccount: String
        var newOrder: String
    }

    struct Order: Codable {
        var status: String
        var finalize: String
        var certificate: String?
    }

    private struct Problem: Codable {
        var type: String?
        var detail: String?
    }
}
