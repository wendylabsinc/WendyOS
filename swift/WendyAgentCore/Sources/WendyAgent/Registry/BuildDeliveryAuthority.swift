import Crypto
import Darwin
import Foundation
import SwiftASN1
import X509

/// One owner for durable admission, accounting and final registry publication.
/// No suspension occurs between the final policy/CAS check and the tag write.
actor BuildDeliveryAuthority {
    typealias Failure = BuildDeliveryContract.Failure
    struct Record: Codable, Sendable {
        var compact: String
        var admittedAt: Int64
        var bytes: Int64 = 0
        var uploads: [String] = []
        var output = ""
        var completed = false
    }
    struct Session: Sendable {
        var verified: BuildDeliveryContract.Verified
        var record: Record
    }
    struct Status: Sendable {
        var reference: String
        var output: String
        var completed: Bool
    }
    private let directory: URL
    private let images: BlobStore
    private let now: @Sendable () -> Int64
    private var sessions: [String: Session] = [:]
    private var jobs: [String: String] = [:]
    private var target = ""
    private var roots: [Certificate] = []
    private var certificates: [Data] = []
    private var ready = false
    private var provisioned = false
    private var operatorScope: OrgIdentity.Scope?
    private var orgMode: ClientCertAuthorizer.OrgEnforcementMode = .grace
    private var generation: UInt64 = 0
    private var lockDescriptor: Int32 = -1

    init(
        directory: URL,
        images: BlobStore,
        now: @escaping @Sendable () -> Int64 = { Int64(Date().timeIntervalSince1970) }
    ) {
        self.directory = directory
        self.images = images
        self.now = now
    }
    deinit { if lockDescriptor >= 0 { _ = Darwin.close(lockDescriptor) } }

    /// Called before listeners start, and after enrollment changes. Persisted
    /// artifacts are reverified at their recorded admission time against the
    /// current local roots, never against a root supplied by the JWS.
    func configure(
        certPEM: String?,
        chainPEM: String?,
        orgMode: ClientCertAuthorizer.OrgEnforcementMode = .grace
    ) async throws {
        generation &+= 1
        let epoch = generation
        ready = false
        sessions = [:]
        jobs = [:]
        roots = []
        target = ""
        certificates = []
        provisioned = certPEM != nil
        operatorScope = nil
        self.orgMode = orgMode
        guard let certPEM, let chainPEM else { return }
        let leaves = ClientCertAuthorizer.parseCertificates(pem: certPEM)
        let chain = ClientCertAuthorizer.parseCertificates(pem: chainPEM)
        guard let leaf = leaves.first, let identity = try OrgIdentity.identity(fromLeaf: leaf)
        else { return }
        operatorScope = identity.scope
        guard let principal = identity.principal else { return }
        guard #available(macOS 26.0, *) else {
            throw Failure("operator delivery verification requires macOS 26 or later")
        }
        _ = try BuildDeliveryContract.principal(principal, kind: "device")
        target = principal
        roots = chain.filter {
            if case .isCertificateAuthority = try? $0.extensions.basicConstraints { return true }
            return false
        }
        guard !roots.isEmpty else { throw Failure("local delivery PKI trust unavailable") }
        certificates = try (leaves + chain).map { certificate in
            var serializer = DER.Serializer()
            try serializer.serialize(certificate)
            return Data(serializer.serializedBytes)
        }
        try acquireLock()
        let files = try FileManager.default.contentsOfDirectory(
            at: directory,
            includingPropertiesForKeys: [.fileSizeKey]
        ).filter { $0.pathExtension == "json" }
        guard files.count <= 4096 else { throw Failure("too many saved delivery records") }
        var recovered: [String: Session] = [:]
        var recoveredJobs: [String: String] = [:]
        for file in files {
            guard
                (try file.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? Int.max) <= 256 * 1024
            else { throw Failure("oversized saved delivery") }
            let record = try JSONDecoder().decode(Record.self, from: Data(contentsOf: file))
            let id = BuildDeliveryContract.sessionID(record.compact)
            guard file.deletingPathExtension().lastPathComponent == id, record.admittedAt > 0,
                record.admittedAt <= now()
            else { throw Failure("invalid saved delivery admission") }
            let verified = try await BuildDeliveryContract.verify(
                record.compact,
                roots: roots,
                target: target,
                at: record.admittedAt
            )
            guard generation == epoch else {
                throw Failure("delivery identity changed during recovery")
            }
            if verified.claims.exp <= now() {
                removeUploads(id, record: record)
                try FileManager.default.removeItem(at: file)
                continue
            }
            guard record.bytes >= 0, record.bytes <= verified.claims.max_bytes,
                record.uploads.count <= 1024,
                Set(record.uploads).count == record.uploads.count,
                record.uploads.allSatisfy({ BuildDeliveryContract.canonicalUUID($0) }),
                record.output.isEmpty || BlobStore.isValidSHA256Digest(record.output),
                !record.completed || !record.output.isEmpty,
                recoveredJobs[verified.claims.jti] == nil
            else { throw Failure("invalid persisted delivery state") }
            recovered[id] = Session(verified: verified, record: record)
            recoveredJobs[verified.claims.jti] = id
        }
        guard recovered.count <= 1024, generation == epoch else {
            throw Failure("delivery state unavailable")
        }
        sessions = recovered
        jobs = recoveredJobs
        ready = true
    }

    private func acquireLock() throws {
        if lockDescriptor >= 0 { return }
        try FileManager.default.createDirectory(
            at: directory,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        // The state root can itself be new on first enrollment. Persist every
        // parent entry before acknowledging any admission in the new store.
        var parent = directory
        while parent.path != "/" {
            try ProvisioningStore.synchronizeDirectory(parent)
            let next = parent.deletingLastPathComponent()
            if next.path == parent.path { break }
            parent = next
        }
        let descriptor = unsafe Darwin.open(
            directory.appendingPathComponent(".lock").path,
            O_CREAT | O_RDWR | O_CLOEXEC,
            0o600
        )
        guard descriptor >= 0 else { throw Failure("cannot open delivery lock") }
        let fileLock: (Int32, Int32) -> Int32 = flock
        guard fileLock(descriptor, LOCK_EX | LOCK_NB) == 0 else {
            _ = Darwin.close(descriptor)
            throw Failure("delivery store is already owned")
        }
        lockDescriptor = descriptor
    }
    func close() {
        generation &+= 1
        ready = false
        sessions = [:]
        jobs = [:]
        target = ""
        roots = []
        certificates = []
        if lockDescriptor >= 0 {
            _ = Darwin.close(lockDescriptor)
            lockDescriptor = -1
        }
    }
    func closeAndRemove() throws {
        for (id, session) in sessions { removeUploads(id, record: session.record) }
        close()
        if FileManager.default.fileExists(atPath: directory.path) {
            try FileManager.default.removeItem(at: directory)
            try ProvisioningStore.synchronizeDirectory(directory.deletingLastPathComponent())
        }
    }
    func capabilities() throws -> (String, [Data]) {
        guard ready else {
            throw Failure(
                "operator delivery requires a provisioned target and durable authorization store"
            )
        }
        return (target, certificates)
    }
    private func removeUploads(_ id: String, record: Record) {
        let directory = images.root.appendingPathComponent("registry-uploads")
        for upload in record.uploads where BuildDeliveryContract.canonicalUUID(upload) {
            try? FileManager.default.removeItem(
                at: directory.appendingPathComponent("delivery-" + id + "-" + upload)
            )
        }
    }
    private func persist(_ id: String, _ record: Record) throws {
        let data = try JSONEncoder().encode(record)
        try ProvisioningStore.writeDurableFile(
            directory.appendingPathComponent(id + ".json"),
            data: data,
            permissions: 0o600
        )
    }
    private func live(_ id: String, leaf: Certificate) throws -> Session {
        guard ready, let session = sessions[id], now() >= session.verified.admittedAt,
            now() < session.verified.claims.exp,
            Date(timeIntervalSince1970: Double(now())) >= leaf.notValidBefore,
            Date(timeIntervalSince1970: Double(now())) < leaf.notValidAfter,
            let identity = try OrgIdentity.identity(fromLeaf: leaf), identity.entityType == "asset",
            identity.principal == session.verified.claims.builder,
            try BuildDeliveryContract.fingerprint(leaf)
                == session.verified.claims.builder_spki_sha256
        else {
            throw Failure("delivery session does not authorize this builder identity, key or time")
        }
        return session
    }
    func policy(
        _ id: String,
        leaf: Certificate,
        repository: String
    ) throws -> BuildDeliveryContract.Claims {
        let session = try live(id, leaf: leaf)
        let ref = try BuildDeliveryContract.reference(session.verified.claims.reference)
        guard ref.repository == repository else {
            throw Failure("delivery does not authorize this repository")
        }
        return session.verified.claims
    }
    func admit(_ compact: String, operator leaf: Certificate) async throws -> String {
        guard ready, let identity = try OrgIdentity.identity(fromLeaf: leaf),
            identity.entityType == "user",
            let issuer = identity.principal,
            Date(timeIntervalSince1970: Double(now())) < leaf.notValidAfter
        else { throw Failure("admission requires the operator's authenticated identity") }
        let id = BuildDeliveryContract.sessionID(compact)
        if let existing = sessions[id] {
            guard existing.record.compact == compact, existing.verified.claims.iss == issuer,
                now() < existing.verified.claims.exp
            else { throw Failure("delivery admission replay mismatch") }
            return id
        }
        let epoch = generation
        let verified = try await BuildDeliveryContract.verify(
            compact,
            roots: roots,
            target: target,
            at: now()
        )
        guard ready, epoch == generation, verified.claims.iss == issuer else {
            throw Failure("delivery identity changed or issuer mismatched")
        }
        // Recheck the live clock after asynchronous certificate verification.
        guard now() >= verified.claims.iat, now() < verified.claims.admit_before,
            now() < verified.claims.exp
        else { throw Failure("delivery admission expired") }
        if let existing = sessions[id] {
            guard existing.record.compact == compact else {
                throw Failure("delivery admission replay mismatch")
            }
            return id
        }
        guard jobs[verified.claims.jti] == nil else {
            throw Failure("job id is already bound to another contract")
        }
        for (key, session) in sessions where session.verified.claims.exp <= now() {
            try FileManager.default.removeItem(at: directory.appendingPathComponent(key + ".json"))
            removeUploads(key, record: session.record)
            jobs.removeValue(forKey: session.verified.claims.jti)
            sessions.removeValue(forKey: key)
        }
        guard sessions.count < 1024 else { throw Failure("too many active delivery sessions") }
        let record = Record(compact: compact, admittedAt: verified.admittedAt)
        try persist(id, record)
        sessions[id] = Session(verified: verified, record: record)
        jobs[verified.claims.jti] = id
        return id
    }
    func reserve(_ id: String, leaf: Certificate, bytes: Int64) throws {
        var session = try live(id, leaf: leaf)
        guard !session.record.completed, bytes >= 0,
            bytes <= session.verified.claims.max_bytes - session.record.bytes
        else {
            throw Failure("delivery byte budget exhausted")
        }
        session.record.bytes += bytes
        try persist(id, session.record)
        sessions[id] = session
    }
    // A completed final-manifest retry only compares bytes and the current tag
    // in publish; it cannot stage content or consume more durable authority.
    func reserveManifest(_ id: String, leaf: Certificate, bytes: Int64) throws {
        if try live(id, leaf: leaf).record.completed { return }
        try reserve(id, leaf: leaf, bytes: bytes)
    }
    func stage(_ id: String, leaf: Certificate, repository: String) throws {
        _ = try policy(id, leaf: leaf, repository: repository)
        guard sessions[id]?.record.completed == false else {
            throw Failure("delivery already completed")
        }
    }
    func newUpload(_ id: String, leaf: Certificate, repository: String) throws -> String {
        try stage(id, leaf: leaf, repository: repository)
        guard var session = sessions[id], session.record.uploads.count < 1024 else {
            throw Failure("delivery upload limit exceeded")
        }
        let upload = UUID().uuidString.lowercased()
        session.record.uploads.append(upload)
        try persist(id, session.record)
        sessions[id] = session
        return "delivery-" + id + "-" + upload
    }
    func checkUpload(
        _ upload: String,
        session id: String,
        leaf: Certificate,
        repository: String
    ) throws {
        try stage(id, leaf: leaf, repository: repository)
        let prefix = "delivery-" + id + "-"
        guard upload.hasPrefix(prefix), let session = sessions[id],
            session.record.uploads.contains(String(upload.dropFirst(prefix.count)))
        else { throw Failure("upload does not belong to this delivery") }
    }
    func referenceDigest(_ reference: String) throws -> String {
        let ref = try BuildDeliveryContract.reference(reference)
        return try images.manifestDigest(repository: ref.repository, reference: ref.tag) ?? ""
    }
    func status(_ id: String, leaf: Certificate) throws -> Status {
        let session = try live(id, leaf: leaf)
        if session.record.completed,
            try referenceDigest(session.verified.claims.reference) != session.record.output
        {
            throw Failure("completed delivery reference was superseded")
        }
        return Status(
            reference: session.verified.claims.reference,
            output: session.record.output,
            completed: session.record.completed
        )
    }
    func publish(
        _ data: Data,
        repository: String,
        reference: String,
        session id: String?,
        leaf: Certificate?
    ) throws {
        if let id, let leaf {
            var session = try live(id, leaf: leaf)
            _ = try policy(id, leaf: leaf, repository: repository)
            let output = BlobStore.contentDigest(data)
            if BlobStore.isValidSHA256Digest(reference) {
                try stage(id, leaf: leaf, repository: repository)
                guard reference == output else { throw Failure("manifest digest mismatch") }
                try images.writeManifest(data, repository: repository, reference: reference)
                return
            }
            guard session.verified.claims.reference == repository + ":" + reference else {
                throw Failure("delivery does not authorize this tag")
            }
            let current =
                try images.manifestDigest(repository: repository, reference: reference) ?? ""
            if session.record.completed {
                guard session.record.output == output, current == output else {
                    throw Failure("completed delivery cannot change or restore a publication")
                }
                return
            }
            guard session.record.output.isEmpty || session.record.output == output else {
                throw Failure("delivery output is already bound")
            }
            guard current == session.verified.claims.expected_digest || current == output else {
                throw Failure("image reference changed after operator authorization")
            }
            if session.record.output.isEmpty {
                session.record.output = output
                try persist(id, session.record)
                sessions[id] = session
            }
            _ = try live(id, leaf: leaf)
            if current != output {
                try images.writeManifest(data, repository: repository, reference: reference)
            }
            session.record.completed = true
            try persist(id, session.record)
            sessions[id] = session
        } else {
            if provisioned {
                try RPCIdentityInterceptor.checkOperator(
                    leaf: leaf,
                    expectedScope: operatorScope,
                    orgMode: orgMode
                )
            }
            if let leaf, Date(timeIntervalSince1970: Double(now())) >= leaf.notValidAfter {
                throw Failure("operator certificate expired")
            }
            try images.writeManifest(data, repository: repository, reference: reference)
        }
    }
}
