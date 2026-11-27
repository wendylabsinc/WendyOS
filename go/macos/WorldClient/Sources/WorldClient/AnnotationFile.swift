import Darwin
import Foundation

/// Bounded private atomic file, with compare-and-swap under a filesystem lock.
/// The checksum detects damage; it is not a substitute for record signatures.
struct AnnotationFile: Sendable {
    let directory: URL
    let scopeID: String
    static let limit = 8 << 20
    private struct Envelope: Codable { let version: Int; let sha256: String; let payload: Data }

    func load() throws -> Data? { try locked { try read(root: $0) } }
    func save(_ payload: Data, replacing oldDigest: String?) throws {
        guard payload.count <= Self.limit / 2 else { throw AnnotationError.outboxFull }
        try locked { root in
            guard try read(root: root).map(WorldCredentials.digest) == oldDigest else { throw AnnotationError.changedOutbox }
            let data = try JSONEncoder().encode(Envelope(version: 1, sha256: WorldCredentials.digest(payload), payload: payload))
            guard data.count <= Self.limit else { throw AnnotationError.outboxFull }
            let temporary = ".note-" + UUID().uuidString
            let file = openat(root, temporary, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
            guard file >= 0 else { throw WorldClientError.insecureCache }
            defer { close(file); unlinkat(root, temporary, 0) }
            try data.withUnsafeBytes { raw in
                guard let base = raw.baseAddress else { throw AnnotationError.invalidOutbox }
                var offset = 0
                while offset < raw.count {
                    let count = Darwin.write(file, base.advanced(by: offset), raw.count - offset)
                    if count < 0 && errno == EINTR { continue }
                    guard count > 0 else { throw WorldClientError.insecureCache }
                    offset += count
                }
            }
            guard fsync(file) == 0, renameat(root, temporary, root, scopeID + ".json") == 0, fsync(root) == 0 else { throw WorldClientError.insecureCache }
        }
    }

    private func locked<T>(_ action: (Int32) throws -> T) throws -> T {
        guard directory.isFileURL, WorldClient.isDigest(scopeID) else { throw AnnotationError.invalidOutbox }
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let root = open(directory.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard root >= 0 else { throw WorldClientError.insecureCache }
        defer { close(root) }
        try check(root, kind: S_IFDIR)
        let lock = openat(root, scopeID + ".lock", O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
        guard lock >= 0 else { throw WorldClientError.insecureCache }
        defer { close(lock) }
        try check(lock, kind: S_IFREG)
        guard flock(lock, LOCK_EX | LOCK_NB) == 0 else { throw AnnotationError.changedOutbox }
        defer { flock(lock, LOCK_UN) }
        return try action(root)
    }

    private func check(_ fd: Int32, kind: mode_t) throws {
        var metadata = stat()
        guard fstat(fd, &metadata) == 0, metadata.st_mode & S_IFMT == kind,
              metadata.st_mode & 0o077 == 0, metadata.st_uid == getuid() else { throw WorldClientError.insecureCache }
    }

    private func read(root: Int32) throws -> Data? {
        let fd = openat(root, scopeID + ".json", O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        if fd < 0 && errno == ENOENT { return nil }
        guard fd >= 0 else { throw WorldClientError.insecureCache }
        defer { close(fd) }
        try check(fd, kind: S_IFREG)
        var metadata = stat()
        guard fstat(fd, &metadata) == 0, metadata.st_size > 0, metadata.st_size <= Self.limit else { throw AnnotationError.invalidOutbox }
        var bytes = Data(count: Int(metadata.st_size))
        try bytes.withUnsafeMutableBytes { raw in
            guard let base = raw.baseAddress else { throw AnnotationError.invalidOutbox }
            var offset = 0
            while offset < raw.count {
                let count = Darwin.read(fd, base.advanced(by: offset), raw.count - offset)
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else { throw AnnotationError.invalidOutbox }
                offset += count
            }
        }
        do {
            let envelope = try JSONDecoder().decode(Envelope.self, from: bytes)
            guard envelope.version == 1, WorldCredentials.digest(envelope.payload) == envelope.sha256 else { throw AnnotationError.invalidOutbox }
            return envelope.payload
        } catch { throw AnnotationError.invalidOutbox }
    }
}
