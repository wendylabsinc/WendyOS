import Darwin
import Foundation

public enum InspectionMediaCacheError: Error, LocalizedError, Sendable, Equatable {
    case expired, capacityExceeded, busy
    public var errorDescription: String? {
        switch self {
        case .expired: "This evidence has passed its original retention deadline."
        case .capacityExceeded: "This evidence exceeds the inspection media cache budget."
        case .busy: "Another inspection media cache operation is in progress."
        }
    }
}

/// Stores digest-verified media under the caller-supplied private scope.
/// Observation expiry and media retention are separate. Loading historical media
/// never makes its observation current or authenticates a mission result.
public actor InspectionMediaCache {
    public let scopeID: String
    private let directory: URL
    private let maxBytes: Int
    private static let entryLimit = 12 << 20
    private static let entryCount = 128

    private struct Entry: Codable {
        let version: Int
        let scopeID: String
        let sha256: String
        let contentType: String
        let retainUntil: Date
        let data: Data
        let nativeReference: NativeEvidenceReference?
    }

    /// maxBytes bounds completed on-disk entries, including JSON/base64 overhead.
    /// One atomic replacement may temporarily occupy one additional entry.
    public init(directory: URL, scopeID: String, maxBytes: Int = 32 << 20) throws {
        guard directory.isFileURL, WorldClient.isDigest(scopeID),
              maxBytes > 0, maxBytes <= 64 << 20 else { throw WorldClientError.invalidCache }
        self.scopeID = scopeID
        self.maxBytes = maxBytes
        self.directory = directory.appendingPathComponent(scopeID, isDirectory: true)
        let parent = try Self.openDirectory(directory)
        close(parent)
        let root = try Self.openDirectory(self.directory)
        close(root)
    }

    /// Missing or evicted bytes return nil; a known expired entry throws .expired.
    public func load(reference: MissionInspectionEvidence, now: Date = Date()) throws -> VerifiedEvidence? {
        try load(reference: reference, native: nil, now: now)
    }

    public func load(reference: NativeEvidenceReference, now: Date = Date()) throws -> VerifiedEvidence? {
        try load(reference: reference.media, native: reference, now: now)
    }

    private func load(reference: MissionInspectionEvidence, native: NativeEvidenceReference?, now: Date) throws -> VerifiedEvidence? {
        try validateTime(now)
        return try locked { root in
            guard let entry = try read(root: root, digest: native?.cacheKey ?? reference.sha256) else { return nil }
            try match(entry, reference, native)
            guard now < entry.retainUntil else { throw InspectionMediaCacheError.expired }
            return VerifiedEvidence(data: entry.data, sha256: entry.sha256,
                                    contentType: entry.contentType, retainUntil: entry.retainUntil)
        }
    }

    /// Call after WorldClient.downloadEvidence. Expected metadata and bytes are
    /// checked again; replacing a cached entry cannot renew its original deadline.
    public func store(_ media: VerifiedEvidence, reference: MissionInspectionEvidence, now: Date = Date()) throws {
        try store(media, reference: reference, native: nil, now: now)
    }

    public func store(_ media: VerifiedEvidence, reference: NativeEvidenceReference, now: Date = Date()) throws {
        try store(media, reference: reference.media, native: reference, now: now)
    }

    private func store(_ media: VerifiedEvidence, reference: MissionInspectionEvidence, native: NativeEvidenceReference?, now: Date) throws {
        try validateTime(now)
        guard media.sha256 == reference.sha256, media.contentType == reference.contentType,
              media.data.count == reference.size, WorldCredentials.digest(media.data) == reference.sha256,
              media.retainUntil.timeIntervalSince1970.isFinite,
              media.retainUntil.timeIntervalSince1970 > 0 else { throw WorldClientError.invalidEvidence }
        guard now < media.retainUntil else { throw InspectionMediaCacheError.expired }
        let key = native?.cacheKey ?? reference.sha256
        try locked { root in
            var until = media.retainUntil
            if let previous = try read(root: root, digest: key) {
                try match(previous, reference, native)
                until = min(until, previous.retainUntil)
                guard now < until else { throw InspectionMediaCacheError.expired }
            }
            let entry = Entry(version: 1, scopeID: scopeID, sha256: reference.sha256,
                              contentType: reference.contentType, retainUntil: until, data: media.data, nativeReference: native)
            let bytes = try JSONEncoder().encode(entry)
            guard bytes.count <= maxBytes, bytes.count <= Self.entryLimit else {
                throw InspectionMediaCacheError.capacityExceeded
            }
            var retained: [(digest: String, until: Date, size: Int)] = []
            for name in try FileManager.default.contentsOfDirectory(atPath: directory.path) {
                if name == ".lock" { continue }
                if name.hasPrefix(".pending-") {
                    // Remove only this cache's interrupted atomic-write files.
                    let file = openat(root, name, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
                    guard file >= 0 else { throw WorldClientError.insecureCache }
                    do { try Self.check(file, kind: S_IFREG) }
                    catch { close(file); throw error }
                    close(file)
                    guard unlinkat(root, name, 0) == 0 else { throw WorldClientError.insecureCache }
                    continue
                }
                guard name.hasSuffix(".json"), WorldClient.isDigest(String(name.dropLast(5))) else {
                    throw WorldClientError.invalidCache
                }
                let digest = String(name.dropLast(5))
                guard let old = try read(root: root, digest: digest) else { throw WorldClientError.invalidCache }
                if digest == key { continue }
                if now >= old.retainUntil && old.nativeReference == nil {
                    guard unlinkat(root, name, 0) == 0 else { throw WorldClientError.insecureCache }
                } else {
                    let size = try readBytes(root: root, name: name)?.count ?? 0
                    retained.append((digest, old.retainUntil, size))
                }
            }
            // Cache eviction does not modify the original capture-node retention.
            retained.sort { $0.until == $1.until ? $0.digest < $1.digest : $0.until < $1.until }
            var total = retained.reduce(bytes.count) { $0 + $1.size }
            while total > maxBytes || retained.count + 1 > Self.entryCount {
                let victim = retained.removeFirst()
                guard unlinkat(root, victim.digest + ".json", 0) == 0 else { throw WorldClientError.insecureCache }
                total -= victim.size
            }
            try write(bytes, root: root, name: key + ".json")
        }
    }

    private func validateTime(_ now: Date) throws {
        guard now.timeIntervalSince1970.isFinite, now.timeIntervalSince1970 > 0 else {
            throw WorldClientError.invalidEvidence
        }
    }

    private func match(_ entry: Entry, _ reference: MissionInspectionEvidence, _ native: NativeEvidenceReference?) throws {
        guard entry.sha256 == reference.sha256, entry.contentType == reference.contentType,
              entry.data.count == reference.size, entry.nativeReference == native else { throw WorldClientError.invalidEvidence }
    }

    private func read(root: Int32, digest: String) throws -> Entry? {
        guard let bytes = try readBytes(root: root, name: digest + ".json") else { return nil }
        do {
            let entry = try JSONDecoder().decode(Entry.self, from: bytes)
            guard entry.version == 1, entry.scopeID == scopeID, (entry.nativeReference?.cacheKey ?? entry.sha256) == digest,
                  WorldCredentials.digest(entry.data) == entry.sha256,
                  (1...(8 << 20)).contains(entry.data.count),
                  ["image/png", "image/jpeg", "audio/wav"].contains(entry.contentType),
                  entry.retainUntil.timeIntervalSince1970.isFinite,
                  entry.retainUntil.timeIntervalSince1970 > 0 else { throw WorldClientError.invalidCache }
            if let native = entry.nativeReference { try match(entry, native.media, native) }
            return entry
        } catch { throw WorldClientError.invalidCache }
    }

    private func locked<T>(_ action: (Int32) throws -> T) throws -> T {
        let root = try Self.openDirectory(directory)
        defer { close(root) }
        let lock = openat(root, ".lock", O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
        guard lock >= 0 else { throw WorldClientError.insecureCache }
        defer { close(lock) }
        try Self.check(lock, kind: S_IFREG)
        guard flock(lock, LOCK_EX | LOCK_NB) == 0 else { throw InspectionMediaCacheError.busy }
        defer { flock(lock, LOCK_UN) }
        return try action(root)
    }

    private static func openDirectory(_ directory: URL) throws -> Int32 {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        let root = open(directory.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard root >= 0 else { throw WorldClientError.insecureCache }
        do { try check(root, kind: S_IFDIR); return root }
        catch { close(root); throw error }
    }

    private static func check(_ fd: Int32, kind: mode_t) throws {
        var metadata = stat()
        guard fstat(fd, &metadata) == 0, metadata.st_mode & S_IFMT == kind,
              metadata.st_mode & 0o077 == 0, metadata.st_uid == getuid(),
              kind != S_IFREG || metadata.st_nlink == 1 else { throw WorldClientError.insecureCache }
    }

    private func readBytes(root: Int32, name: String) throws -> Data? {
        let file = openat(root, name, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        if file < 0 && errno == ENOENT { return nil }
        guard file >= 0 else { throw WorldClientError.insecureCache }
        defer { close(file) }
        try Self.check(file, kind: S_IFREG)
        var metadata = stat()
        guard fstat(file, &metadata) == 0, metadata.st_size > 0,
              metadata.st_size <= Self.entryLimit else { throw WorldClientError.invalidCache }
        var bytes = Data(count: Int(metadata.st_size))
        try bytes.withUnsafeMutableBytes { raw in
            guard let base = raw.baseAddress else { throw WorldClientError.invalidCache }
            var offset = 0
            while offset < raw.count {
                let count = Darwin.read(file, base.advanced(by: offset), raw.count - offset)
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else { throw WorldClientError.invalidCache }
                offset += count
            }
        }
        return bytes
    }

    private func write(_ bytes: Data, root: Int32, name: String) throws {
        let temporary = ".pending-" + UUID().uuidString
        let file = openat(root, temporary, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
        guard file >= 0 else { throw WorldClientError.insecureCache }
        defer { close(file); unlinkat(root, temporary, 0) }
        try bytes.withUnsafeBytes { raw in
            guard let base = raw.baseAddress else { throw WorldClientError.invalidCache }
            var offset = 0
            while offset < raw.count {
                let count = Darwin.write(file, base.advanced(by: offset), raw.count - offset)
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else { throw WorldClientError.insecureCache }
                offset += count
            }
        }
        guard fsync(file) == 0, renameat(root, temporary, root, name) == 0,
              fsync(root) == 0 else { throw WorldClientError.insecureCache }
    }
}
