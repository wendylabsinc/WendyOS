import Crypto
import Foundation
import Testing

@testable import WendyAgentCore

@Suite struct BlobStoreTests {
    private func tempRoot() -> URL {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("blobstore-\(UUID().uuidString)")
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    @Test func writeAndReadBlobByDigest() throws {
        let store = BlobStore(root: tempRoot())
        let payload = Data("hello".utf8)
        let hex = SHA256.hash(data: payload).map { String(format: "%02x", $0) }.joined()
        let digest = "sha256:\(hex)"
        try store.writeBlob(payload, expectedDigest: digest)
        #expect(store.hasBlob(digest: digest))
        #expect(try Data(contentsOf: store.blobURL(digest: digest)) == payload)
    }

    @Test func writeBlobRejectsDigestMismatch() {
        let store = BlobStore(root: tempRoot())
        #expect(throws: (any Error).self) {
            try store.writeBlob(Data("hello".utf8), expectedDigest: "sha256:deadbeef")
        }
    }

    @Test func manifestRoundTripByTag() throws {
        let store = BlobStore(root: tempRoot())
        let manifest = Data(#"{"schemaVersion":2}"#.utf8)
        try store.writeManifest(manifest, repository: "app", reference: "latest")
        let url = try #require(store.manifestURL(repository: "app", reference: "latest"))
        #expect(try Data(contentsOf: url) == manifest)
    }

    @Test func manifestRoundTripByContentDigest() throws {
        let store = BlobStore(root: tempRoot())
        let manifest = Data(#"{"schemaVersion":2}"#.utf8)
        try store.writeManifest(manifest, repository: "app", reference: "latest")
        let hex = SHA256.hash(data: manifest).map { String(format: "%02x", $0) }.joined()
        let url = try #require(store.manifestURL(repository: "app", reference: "sha256:\(hex)"))
        #expect(try Data(contentsOf: url) == manifest)
    }

    @Test func digestStagingCannotOverwriteAnAliasingTag() throws {
        let root = tempRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let store = BlobStore(root: root)
        let child = Data("staged child manifest".utf8)
        let digest = BlobStore.contentDigest(child)
        let tag = digest.replacingOccurrences(of: ":", with: "_")
        let ambient = Data("unrelated operator publication".utf8)
        try store.writeManifest(ambient, repository: "app", reference: tag)
        try store.writeManifest(child, repository: "app", reference: digest)
        let tagURL = try #require(store.manifestURL(repository: "app", reference: tag))
        let digestURL = try #require(store.manifestURL(repository: "app", reference: digest))
        #expect(tagURL != digestURL)
        #expect(try Data(contentsOf: tagURL) == ambient)
        #expect(try Data(contentsOf: digestURL) == child)
    }

    @Test func legacyManifestReadsVerifyDigestBeforeUsingAnAmbiguousFilename() throws {
        let root = tempRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let store = BlobStore(root: root)
        let directory = root.appendingPathComponent("manifests/app")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let data = Data("legacy manifest".utf8)
        let digest = BlobStore.contentDigest(data)
        try data.write(to: directory.appendingPathComponent("latest"))
        try data.write(
            to: directory.appendingPathComponent(digest.replacingOccurrences(of: ":", with: "_"))
        )
        #expect(store.manifestURL(repository: "app", reference: "latest") != nil)
        #expect(store.manifestURL(repository: "app", reference: digest) != nil)
        let wrong = "sha256:" + String(repeating: "a", count: 64)
        try data.write(
            to: directory.appendingPathComponent(wrong.replacingOccurrences(of: ":", with: "_"))
        )
        #expect(store.manifestURL(repository: "app", reference: wrong) == nil)
    }

    // MARK: - Digest validation (path traversal fix)

    @Test func isValidSHA256DigestAcceptsRealDigest() {
        let payload = Data("hello".utf8)
        let hex = SHA256.hash(data: payload).map { String(format: "%02x", $0) }.joined()
        #expect(BlobStore.isValidSHA256Digest("sha256:\(hex)"))
    }

    @Test func isValidSHA256DigestRejectsColonStyleTraversal() {
        // The confirmed exploit shape: a Hummingbird path segment like
        // `x:..:..:..:..:..:..:..:etc:passwd` that `blobURL` would otherwise
        // turn into `blobs/x/../../../../../../../etc/passwd`.
        #expect(!BlobStore.isValidSHA256Digest("x:..:..:..:etc:passwd"))
    }

    @Test func isValidSHA256DigestRejectsSlashStyleTraversal() {
        #expect(!BlobStore.isValidSHA256Digest("sha256:../../../../etc/passwd"))
    }

    @Test func isValidSHA256DigestRejectsUppercaseHex() {
        let hex = String(repeating: "A", count: 64)
        #expect(!BlobStore.isValidSHA256Digest("sha256:\(hex)"))
    }

    @Test func isValidSHA256DigestRejectsShortHex() {
        let hex = String(repeating: "a", count: 63)
        #expect(!BlobStore.isValidSHA256Digest("sha256:\(hex)"))
    }

    @Test func isValidSHA256DigestRejectsLongHex() {
        let hex = String(repeating: "a", count: 65)
        #expect(!BlobStore.isValidSHA256Digest("sha256:\(hex)"))
    }

    @Test func isValidSHA256DigestRejectsMissingPrefix() {
        let hex = String(repeating: "a", count: 64)
        #expect(!BlobStore.isValidSHA256Digest(hex))
    }

    @Test func hasBlobFalseForNeverWrittenValidDigest() {
        let store = BlobStore(root: tempRoot())
        let hex = String(repeating: "a", count: 64)
        #expect(!store.hasBlob(digest: "sha256:\(hex)"))
    }

    @Test func hasBlobFalseForTraversalDigest() {
        let store = BlobStore(root: tempRoot())
        #expect(!store.hasBlob(digest: "x:..:..:..:..:..:..:..:etc:passwd"))
    }

    @Test func readBlobNilForTraversalDigest() {
        let store = BlobStore(root: tempRoot())
        #expect(store.readBlob(digest: "x:..:..:..:..:..:..:..:etc:passwd") == nil)
    }

    @Test func writeBlobRejectsTraversalExpectedDigest() {
        let store = BlobStore(root: tempRoot())
        #expect(throws: BlobStore.BlobError.self) {
            try store.writeBlob(Data("hello".utf8), expectedDigest: "x:..:..:..:etc:passwd")
        }
    }

    // MARK: - Repository/reference validation (manifest traversal fix)

    @Test func isValidRepositoryAcceptsNormalName() {
        #expect(BlobStore.isValidRepository("my-app_2.0"))
    }

    @Test func isValidRepositoryRejectsParentTraversal() {
        #expect(!BlobStore.isValidRepository(".."))
    }

    @Test func isValidRepositoryRejectsEmbeddedTraversal() {
        #expect(!BlobStore.isValidRepository("a/../../etc"))
    }

    @Test func isValidRepositoryRejectsSlash() {
        #expect(!BlobStore.isValidRepository("a/b"))
    }

    @Test func isValidRepositoryRejectsEmpty() {
        #expect(!BlobStore.isValidRepository(""))
    }

    @Test func isValidReferenceAcceptsTag() {
        #expect(BlobStore.isValidReference("latest"))
    }

    @Test func isValidReferenceAcceptsDigest() {
        let hex = String(repeating: "a", count: 64)
        #expect(BlobStore.isValidReference("sha256:\(hex)"))
    }

    @Test func isValidReferenceRejectsParentTraversal() {
        #expect(!BlobStore.isValidReference(".."))
    }

    @Test func isValidReferenceRejectsSlash() {
        #expect(!BlobStore.isValidReference("a/b"))
    }

    @Test func isValidReferenceRejectsEmpty() {
        #expect(!BlobStore.isValidReference(""))
    }
}
