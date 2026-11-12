import Crypto
import Foundation
import HTTPTypes
import Hummingbird

/// Payloads stream to disk. Upload ownership is checked before opening a file;
/// delegated IDs are issued and persisted by the durable admission authority.
actor RegistryUploads {
    private struct Owner {
        var peer: String
        var repository: String
        var expires: Int64
    }
    private let directory: URL
    private let store: BlobStore
    private let authority: BuildDeliveryAuthority?
    private var ambient: [String: Owner] = [:]
    private var busy: Set<String> = []
    private static let maximumSize: Int64 = 8 * 1024 * 1024 * 1024
    init(store: BlobStore, authority: BuildDeliveryAuthority?) {
        self.store = store
        self.authority = authority
        directory = store.root.appendingPathComponent("registry-uploads")
    }
    func start(access: RegistryAccess, repository: String) async throws -> String {
        try access.checkTime()
        try FileManager.default.createDirectory(
            at: directory,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        try ProvisioningStore.synchronizeDirectory(store.root)
        let id: String
        if let session = access.session, let leaf = access.leaf, let authority {
            id = try await authority.newUpload(session, leaf: leaf, repository: repository)
        } else {
            let now = Int64(Date().timeIntervalSince1970)
            // Ambient sessions do not survive a process restart. Reclaim their
            // old disk files on the next upload, without touching delegated IDs.
            if let files = try? FileManager.default.contentsOfDirectory(
                at: directory,
                includingPropertiesForKeys: [.contentModificationDateKey]
            ) {
                for file in files
                where BuildDeliveryContract.canonicalUUID(file.lastPathComponent)
                    && ambient[file.lastPathComponent] == nil
                    && !busy.contains(file.lastPathComponent)
                {
                    if let date = try? file.resourceValues(forKeys: [.contentModificationDateKey])
                        .contentModificationDate, date.timeIntervalSince1970 < Double(now - 900)
                    {
                        try? FileManager.default.removeItem(at: file)
                    }
                }
            }
            for (id, owner) in ambient where owner.expires <= now && !busy.contains(id) {
                try? FileManager.default.removeItem(at: directory.appendingPathComponent(id))
                ambient.removeValue(forKey: id)
            }
            guard ambient.count < 1024 else { throw HTTPError(.tooManyRequests) }
            id = UUID().uuidString.lowercased()
            ambient[id] = Owner(
                peer: access.owner,
                repository: repository,
                expires: min(now + 900, access.deadline ?? Int64.max)
            )
        }
        guard
            FileManager.default.createFile(
                atPath: directory.appendingPathComponent(id).path,
                contents: Data(),
                attributes: [.posixPermissions: 0o600]
            )
        else { throw HTTPError(.internalServerError) }
        let file = try FileHandle(forWritingTo: directory.appendingPathComponent(id))
        try file.synchronize()
        try file.close()
        try ProvisioningStore.synchronizeDirectory(directory)
        return id
    }
    private func authorize(
        _ id: String,
        access: RegistryAccess,
        repository: String
    ) async throws -> URL {
        try access.checkTime()
        guard id.utf8.count <= 128, !id.isEmpty,
            id.utf8.allSatisfy({
                (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0)
                    || $0 == 45 || $0 == 95
            })
        else { throw HTTPError(.notFound) }
        if let session = access.session, let leaf = access.leaf, let authority {
            try await authority.checkUpload(
                id,
                session: session,
                leaf: leaf,
                repository: repository
            )
        } else {
            guard let owner = ambient[id], owner.peer == access.owner,
                owner.repository == repository,
                owner.expires > Int64(Date().timeIntervalSince1970)
            else { throw HTTPError(.notFound) }
        }
        let url = directory.appendingPathComponent(id)
        guard FileManager.default.fileExists(atPath: url.path) else { throw HTTPError(.notFound) }
        return url
    }
    func size(_ id: String, access: RegistryAccess, repository: String) async throws -> Int64 {
        let url = try await authorize(id, access: access, repository: repository)
        guard
            let size = try FileManager.default.attributesOfItem(atPath: url.path)[.size]
                as? NSNumber
        else { throw HTTPError(.internalServerError) }
        return size.int64Value
    }
    func append(
        _ id: String,
        request: Request,
        access: RegistryAccess,
        repository: String
    ) async throws -> Int64 {
        let url = try await authorize(id, access: access, repository: repository)
        guard busy.insert(id).inserted else {
            throw HTTPError(.conflict, message: "upload already receiving data")
        }
        defer { busy.remove(id) }
        let handle = try FileHandle(forUpdating: url)
        defer { try? handle.close() }
        var total = Int64(try handle.seekToEnd())
        if let range = request.headers[HTTPField.Name("Content-Range")!] {
            let first = range.replacingOccurrences(of: "bytes ", with: "").split(
                separator: "-",
                maxSplits: 1
            ).first
            guard first.flatMap({ Int64($0) }) == total else {
                throw HTTPError(.rangeNotSatisfiable)
            }
        }
        let declared: Int64?
        if let length = request.headers[.contentLength] {
            guard let count = Int64(length), count >= 0, count <= Self.maximumSize - total else {
                throw HTTPError(.contentTooLarge)
            }
            declared = count
            if let session = access.session, let leaf = access.leaf, let authority {
                try await authority.reserve(session, leaf: leaf, bytes: count)
            }
        } else {
            declared = nil
        }
        var received: Int64 = 0
        for try await buffer in request.body {
            try Task.checkCancellation()
            try access.checkTime()
            let count = Int64(buffer.readableBytes)
            guard count <= Self.maximumSize - total,
                declared == nil || count <= declared! - received
            else { throw HTTPError(.contentTooLarge) }
            if let session = access.session, let leaf = access.leaf, let authority {
                if declared == nil {
                    try await authority.reserve(session, leaf: leaf, bytes: count)
                } else {
                    _ = try await authority.policy(session, leaf: leaf, repository: repository)
                }
            }
            if let session = access.session, let leaf = access.leaf, let authority {
                try await authority.stage(session, leaf: leaf, repository: repository)
            }
            try access.checkTime()
            try handle.write(contentsOf: Data(buffer.readableBytesView))
            total += count
            received += count
        }
        if let declared, received != declared {
            throw HTTPError(.badRequest, message: "incomplete upload body")
        }
        try handle.synchronize()
        return total
    }
    func commit(
        _ id: String,
        digest: String,
        access: RegistryAccess,
        repository: String
    ) async throws {
        let url = try await authorize(id, access: access, repository: repository)
        guard !busy.contains(id), BlobStore.isValidSHA256Digest(digest) else {
            throw HTTPError(.badRequest)
        }
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        var hash = SHA256()
        while let data = try handle.read(upToCount: 64 * 1024), !data.isEmpty {
            try Task.checkCancellation()
            try access.checkTime()
            hash.update(data: data)
        }
        let actual = "sha256:" + hash.finalize().map { String(format: "%02x", $0) }.joined()
        guard actual == digest else {
            throw HTTPError(.badRequest, message: "blob digest mismatch")
        }
        try access.checkTime()
        let destination = store.blobURL(digest: actual)
        try FileManager.default.createDirectory(
            at: destination.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        if FileManager.default.fileExists(atPath: destination.path) {
            try FileManager.default.removeItem(at: url)
        } else {
            try FileManager.default.moveItem(at: url, to: destination)
        }
        try ProvisioningStore.synchronizeDirectory(destination.deletingLastPathComponent())
        try ProvisioningStore.synchronizeDirectory(
            destination.deletingLastPathComponent().deletingLastPathComponent()
        )
        try ProvisioningStore.synchronizeDirectory(store.root)
        ambient.removeValue(forKey: id)
    }
}
