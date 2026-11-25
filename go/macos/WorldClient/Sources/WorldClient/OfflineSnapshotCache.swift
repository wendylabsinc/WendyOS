import Darwin
import Foundation

/// A private, bounded cache of read responses. The digest detects corruption;
/// it is not a signature or proof of an observation's original author.
public struct OfflineSnapshotCache: Sendable {
    public let directory: URL
    private static let limit = 64 << 20
    private struct Envelope: Codable {
        let version: Int
        let sha256: String
        let payload: Data
    }

    public init(directory: URL) { self.directory = directory }

    public func save(_ model: CachedWorldReadModel) throws {
        try validate(model)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .millisecondsSince1970
        let payload = try encoder.encode(model)
        guard payload.count <= Self.limit else { throw WorldClientError.responseTooLarge }
        let data = try encoder.encode(Envelope(version: 1, sha256: WorldCredentials.digest(payload), payload: payload))
        guard data.count <= Self.limit else { throw WorldClientError.responseTooLarge }
        guard let root = try openDirectory(create: true) else { throw WorldClientError.insecureCache }
        defer { close(root) }
        let temporary = ".pending-" + UUID().uuidString
        let file = openat(root, temporary, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
        guard file >= 0 else { throw WorldClientError.insecureCache }
        defer { close(file); unlinkat(root, temporary, 0) }
        try data.withUnsafeBytes { raw in
            guard let base = raw.baseAddress else { throw WorldClientError.invalidCache }
            var offset = 0
            while offset < raw.count {
                let count = Darwin.write(file, base.advanced(by: offset), raw.count - offset)
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else { throw WorldClientError.insecureCache }
                offset += count
            }
        }
        guard fsync(file) == 0,
              renameat(root, temporary, root, model.scopeID + ".json") == 0,
              fsync(root) == 0 else { throw WorldClientError.insecureCache }
    }

    public func load(scopeID: String, readMode: WorldReadMode = .full) throws -> CachedWorldReadModel? {
        guard WorldClient.isDigest(scopeID) else { throw WorldClientError.invalidCache }
        guard let root = try openDirectory(create: false) else { return nil }
        defer { close(root) }
        let file = openat(root, scopeID + ".json", O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        if file < 0 && errno == ENOENT { return nil }
        guard file >= 0 else { throw WorldClientError.insecureCache }
        defer { close(file) }
        var metadata = stat()
        guard fstat(file, &metadata) == 0,
              metadata.st_mode & S_IFMT == S_IFREG,
              metadata.st_mode & 0o077 == 0, metadata.st_uid == getuid(),
              metadata.st_size > 0, metadata.st_size <= Self.limit else { throw WorldClientError.insecureCache }
        var data = Data(count: Int(metadata.st_size))
        try data.withUnsafeMutableBytes { raw in
            guard let base = raw.baseAddress else { throw WorldClientError.invalidCache }
            var offset = 0
            while offset < raw.count {
                let count = Darwin.read(file, base.advanced(by: offset), raw.count - offset)
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else { throw WorldClientError.invalidCache }
                offset += count
            }
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .millisecondsSince1970
        do {
            let envelope = try decoder.decode(Envelope.self, from: data)
            guard envelope.version == 1, envelope.sha256 == WorldCredentials.digest(envelope.payload) else { throw WorldClientError.invalidCache }
            let model = try decoder.decode(CachedWorldReadModel.self, from: envelope.payload)
            guard model.scopeID == scopeID else { throw WorldClientError.invalidCache }
            try validate(model)
            guard model.readMode == readMode else { return nil }
            return model
        } catch let error as WorldClientError { throw error }
        catch { throw WorldClientError.invalidCache }
    }

    private func validate(_ model: CachedWorldReadModel) throws {
        guard WorldClient.isDigest(model.scopeID), model.fetchedAt.timeIntervalSince1970.isFinite,
              model.fetchedAt.timeIntervalSince1970 > 0, model.fetchedAt <= Date().addingTimeInterval(60) else { throw WorldClientError.invalidCache }
        if model.readMode == .nativeInference {
            guard model.sites.object?.isEmpty == true, model.tasks.object?.isEmpty == true else { throw WorldClientError.invalidCache }
            return
        }
        guard
              model.sites["publications"]?.array != nil,
              model.tasks["entries"]?.array != nil, model.tasks["last_known"]?.bool == true else { throw WorldClientError.invalidCache }
    }

    private func openDirectory(create: Bool) throws -> Int32? {
        guard directory.isFileURL else { throw WorldClientError.insecureCache }
        if create {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        }
        let root = open(directory.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        if root < 0 && errno == ENOENT && !create { return nil }
        guard root >= 0 else { throw WorldClientError.insecureCache }
        var metadata = stat()
        guard fstat(root, &metadata) == 0, metadata.st_mode & S_IFMT == S_IFDIR,
              metadata.st_mode & 0o077 == 0, metadata.st_uid == getuid() else {
            close(root)
            throw WorldClientError.insecureCache
        }
        return root
    }
}
