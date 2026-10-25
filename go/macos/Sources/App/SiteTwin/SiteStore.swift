import Foundation

public struct StoredSite: Sendable, Identifiable {
    public var site: Site
    public var modelURL: URL
    public var visualMapURL: URL? = nil
    public var id: UUID { site.id }
}

/// Site data contains no connection credentials. Import never initiates network
/// access; the host app supplies cameras from its authenticated connection.
public actor SiteStore {
    public static let maximumModelBytes = 128 * 1024 * 1024
    private let directory: URL

    public init(directory: URL) { self.directory = directory }

    public static func applicationStore() throws -> SiteStore {
        let base = try FileManager.default.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        return SiteStore(directory: base.appendingPathComponent("WendySites", isDirectory: true))
    }

    public func list() throws -> [StoredSite] {
        guard FileManager.default.fileExists(atPath: directory.path) else { return [] }
        return try FileManager.default.contentsOfDirectory(
            at: directory,
            includingPropertiesForKeys: nil
        )
        .filter { UUID(uuidString: $0.lastPathComponent) != nil }
        .map { try load(id: UUID(uuidString: $0.lastPathComponent)!) }
        .sorted { $0.site.name.localizedStandardCompare($1.site.name) == .orderedAscending }
    }

    public func load(id: UUID) throws -> StoredSite {
        try read(id: id, folder: folder(id))
    }

    private func read(id: UUID, folder: URL) throws -> StoredSite {
        let manifest = folder.appendingPathComponent("site.json")
        let size = try manifest.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
        guard size <= 1024 * 1024 else { throw SiteError.invalidSite }
        let site = try JSONDecoder().decode(Site.self, from: Data(contentsOf: manifest))
        try site.validate()
        guard site.id == id else { throw SiteError.invalidSite }
        let model = folder.appendingPathComponent("model.usdz")
        try Self.validateModel(model)
        let map = folder.appendingPathComponent("visual-map.json")
        return StoredSite(site: site, modelURL: model, visualMapURL: FileManager.default.fileExists(atPath: map.path) ? map : nil)
    }

    /// Removal is a same-volume move. Keep the original model and placements
    /// recoverable across app restarts; never overwrite another capture.
    public func remove(id: UUID) throws {
        let destination = removedDirectory.appendingPathComponent(id.uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: removedDirectory, withIntermediateDirectories: true)
        try FileManager.default.moveItem(at: folder(id), to: destination)
    }

    public func removed() throws -> [StoredSite] {
        guard FileManager.default.fileExists(atPath: removedDirectory.path) else { return [] }
        return try FileManager.default.contentsOfDirectory(at: removedDirectory, includingPropertiesForKeys: nil)
            .compactMap { url -> StoredSite? in
                guard let id = UUID(uuidString: url.lastPathComponent) else { return nil }
                return try read(id: id, folder: url)
            }
            .sorted { $0.site.name.localizedStandardCompare($1.site.name) == .orderedAscending }
    }

    public func restore(id: UUID) throws -> StoredSite {
        let source = removedDirectory.appendingPathComponent(id.uuidString, isDirectory: true)
        _ = try read(id: id, folder: source)
        try FileManager.default.moveItem(at: source, to: folder(id))
        return try load(id: id)
    }

    private var removedDirectory: URL {
        directory.appendingPathComponent("Recently Removed", isDirectory: true)
    }

    public func create(name: String, model: URL) throws -> StoredSite {
        let site = Site(name: name)
        try site.validate()
        try Self.validateModel(model)
        let destination = folder(site.id)
        try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: true)
        do {
            try FileManager.default.copyItem(
                at: model,
                to: destination.appendingPathComponent("model.usdz")
            )
            try save(site)
            return try load(id: site.id)
        } catch {
            try? FileManager.default.removeItem(at: destination)
            throw error
        }
    }

    public func save(_ site: Site) throws {
        try site.validate()
        guard
            FileManager.default.fileExists(
                atPath: folder(site.id).appendingPathComponent("model.usdz").path
            )
        else { throw SiteError.missingSite }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try encoder.encode(site).write(
            to: folder(site.id).appendingPathComponent("site.json"),
            options: .atomic
        )
    }

    public func archive(id: UUID) throws -> SiteArchive {
        let saved = try load(id: id)
        let model = try Data(contentsOf: saved.modelURL)
        let map = try saved.visualMapURL.map { try SiteVisualMap.read($0, revision: saved.site.modelRevision, model: model) }
        return try SiteArchive(site: saved.site, model: model, visualMap: map)
    }

    /// Imports a copy without overwriting a local site. Placements remain in the
    /// same model revision and gateway identities still require authentication.
    public func importArchive(_ archive: SiteArchive) throws -> StoredSite {
        var site = archive.site
        site.id = UUID()
        try site.validate()
        let destination = folder(site.id)
        try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: true)
        do {
            try archive.model.write(
                to: destination.appendingPathComponent("model.usdz"),
                options: .atomic
            )
            if let map = archive.visualMap {
                try JSONEncoder().encode(map).write(to: destination.appendingPathComponent("visual-map.json"), options: .atomic)
            }
            try save(site)
            return try load(id: site.id)
        } catch {
            try? FileManager.default.removeItem(at: destination)
            throw error
        }
    }

    private func folder(_ id: UUID) -> URL {
        directory.appendingPathComponent(id.uuidString, isDirectory: true)
    }

    public nonisolated static func validateModel(_ url: URL) throws {
        guard url.isFileURL, url.pathExtension.lowercased() == "usdz" else {
            throw SiteError.invalidModel
        }
        let values = try url.resourceValues(forKeys: [
            .fileSizeKey, .isRegularFileKey, .isSymbolicLinkKey,
        ])
        guard values.isRegularFile == true, values.isSymbolicLink != true else {
            throw SiteError.invalidModel
        }
        guard let count = values.fileSize, count > 0, count <= Self.maximumModelBytes else {
            throw SiteError.tooLarge
        }
        let file = try FileHandle(forReadingFrom: url)
        defer { try? file.close() }
        guard try file.read(upToCount: 4) == Data([0x50, 0x4b, 0x03, 0x04]) else {
            throw SiteError.invalidModel
        }
    }
}
