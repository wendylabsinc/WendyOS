import Crypto
import Foundation

/// On-disk content store shared by the registry and the WriteLayer RPC.
/// Blobs live at `<root>/blobs/sha256/<hex>`; manifests at
/// `<root>/manifests/<repo>/<tag>`, with digest entries in a distinct
/// `@digests` directory so an immutable digest cannot alias a mutable tag.
struct BlobStore: Sendable {
    let root: URL

    init(root: URL) {
        self.root = root
        try? FileManager.default.createDirectory(
            at: root.appendingPathComponent("blobs/sha256"),
            withIntermediateDirectories: true
        )
    }

    enum BlobError: Error {
        case digestMismatch(expected: String, actual: String)
        case badDigest(String)
    }

    /// Characters allowed in an OCI repository name or tag reference, beyond
    /// alphanumerics: `.`, `_`, `-`. No `/` (repositories here are a single
    /// path segment) and no other punctuation that could carry a path
    /// traversal or escape the intended directory.
    private static let nameCharacters = CharacterSet(
        charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
    )

    /// Validates that `s` is exactly `sha256:` followed by 64 lowercase hex
    /// characters — nothing else. This is the sole gate before any digest is
    /// used to build a filesystem path; no traversal character (`/`, `.`,
    /// backslash, etc.) can pass.
    static func isValidSHA256Digest(_ s: String) -> Bool {
        guard s.hasPrefix("sha256:") else { return false }
        let hex = s.dropFirst("sha256:".count)
        guard hex.count == 64 else { return false }
        return hex.allSatisfy { ("0"..."9").contains($0) || ("a"..."f").contains($0) }
    }

    /// Validates an OCI repository name: non-empty, a single path segment
    /// (no `/`), no `..`, and restricted to `[a-zA-Z0-9._-]`.
    static func isValidRepository(_ s: String) -> Bool {
        guard !s.isEmpty, s != ".", s != ".." else { return false }
        guard !s.contains("/"), !s.contains("..") else { return false }
        return s.unicodeScalars.allSatisfy { nameCharacters.contains($0) }
    }

    /// Validates an OCI manifest reference: either a valid tag
    /// (`[a-zA-Z0-9._-]{1,128}`, not `.`/`..`) or a valid `sha256:` digest.
    static func isValidReference(_ s: String) -> Bool {
        if isValidSHA256Digest(s) { return true }
        guard !s.isEmpty, s.count <= 128, s != ".", s != ".." else { return false }
        guard !s.contains("/"), !s.contains("..") else { return false }
        return s.unicodeScalars.allSatisfy { nameCharacters.contains($0) }
    }

    func blobURL(digest: String) -> URL {
        root.appendingPathComponent("blobs/\(digest.replacingOccurrences(of: ":", with: "/"))")
    }

    func hasBlob(digest: String) -> Bool {
        guard Self.isValidSHA256Digest(digest) else { return false }
        return FileManager.default.fileExists(atPath: blobURL(digest: digest).path)
    }

    /// Reads a blob by digest, rejecting invalid/traversal digests before
    /// touching the filesystem.
    func readBlob(digest: String) -> Data? {
        guard hasBlob(digest: digest) else { return nil }
        return try? Data(contentsOf: blobURL(digest: digest))
    }

    func writeBlob(_ data: Data, expectedDigest: String) throws {
        let normalizedExpected = expectedDigest.lowercased()
        guard Self.isValidSHA256Digest(normalizedExpected) else {
            throw BlobError.badDigest(expectedDigest)
        }
        let hex = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
        let actual = "sha256:\(hex)"
        guard actual == normalizedExpected else {
            throw BlobError.digestMismatch(expected: expectedDigest, actual: actual)
        }
        // The digest we write under is always our own computed hash, never
        // caller input, so this can never be a traversal path — but assert
        // it stays that way as the invariant this code relies on.
        assert(Self.isValidSHA256Digest(actual))
        let url = blobURL(digest: actual)
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try data.write(to: url, options: .atomic)
    }

    func manifestURL(repository: String, reference: String) -> URL? {
        guard Self.isValidRepository(repository), Self.isValidReference(reference) else {
            return nil
        }
        let url = manifestPath(repository: repository, reference: reference)
        if FileManager.default.fileExists(atPath: url.path) { return url }
        // Read existing installations without writing ambiguous legacy names.
        let legacy = root.appendingPathComponent("manifests").appendingPathComponent(repository)
            .appendingPathComponent(reference.replacingOccurrences(of: ":", with: "_"))
        guard FileManager.default.fileExists(atPath: legacy.path) else { return nil }
        if Self.isValidSHA256Digest(reference) {
            guard let data = try? Data(contentsOf: legacy), data.count <= 4 * 1024 * 1024,
                Self.contentDigest(data) == reference
            else { return nil }
        }
        return legacy
    }

    static func contentDigest(_ data: Data) -> String {
        "sha256:" + SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    func manifestDigest(repository: String, reference: String) throws -> String? {
        guard let url = manifestURL(repository: repository, reference: reference) else {
            return nil
        }
        let data = try Data(contentsOf: url)
        guard data.count <= 4 * 1024 * 1024 else {
            throw BlobError.badDigest("oversized saved manifest")
        }
        return Self.contentDigest(data)
    }

    func writeManifest(_ data: Data, repository: String, reference: String) throws {
        guard Self.isValidRepository(repository), Self.isValidReference(reference),
            data.count <= 4 * 1024 * 1024
        else { throw BlobError.badDigest("invalid manifest reference or size") }
        let url = manifestPath(repository: repository, reference: reference)
        let digest = Self.contentDigest(data)
        if Self.isValidSHA256Digest(reference), reference != digest {
            throw BlobError.digestMismatch(expected: reference, actual: digest)
        }
        let byDigest = manifestPath(repository: repository, reference: digest)
        for directory in [url.deletingLastPathComponent(), byDigest.deletingLastPathComponent()] {
            try FileManager.default.createDirectory(
                at: directory,
                withIntermediateDirectories: true
            )
        }
        let repositoryDirectory = root.appendingPathComponent("manifests").appendingPathComponent(
            repository
        )
        try ProvisioningStore.synchronizeDirectory(repositoryDirectory)
        try ProvisioningStore.synchronizeDirectory(repositoryDirectory.deletingLastPathComponent())
        try ProvisioningStore.synchronizeDirectory(root)
        // Persist digest-addressed content before changing the tag.
        try ProvisioningStore.writeDurableFile(byDigest, data: data, permissions: 0o600)
        if url != byDigest {
            try ProvisioningStore.writeDurableFile(url, data: data, permissions: 0o600)
        }
    }

    private func manifestPath(repository: String, reference: String) -> URL {
        let directory = root.appendingPathComponent("manifests").appendingPathComponent(repository)
        if Self.isValidSHA256Digest(reference) {
            return directory.appendingPathComponent("@digests").appendingPathComponent(
                String(reference.dropFirst("sha256:".count))
            )
        }
        return directory.appendingPathComponent(reference)
    }
}
