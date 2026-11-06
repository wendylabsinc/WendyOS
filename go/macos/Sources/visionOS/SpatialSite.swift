import Foundation

struct SpatialSite: Codable, Sendable {
    let name: String
    let modelURL: URL
    let metresPerUnit: Float
    let cameras: [SiteCamera]
    var sourceArchiveURL: URL? = nil

    static func importFolder() throws -> URL {
        let base = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        let folder = base.appendingPathComponent("SpatialImports")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        return folder
    }
    func remember() throws {
        let folder = try Self.importFolder()
        let stored = Self(name: name, modelURL: URL(fileURLWithPath: modelURL.lastPathComponent), metresPerUnit: metresPerUnit, cameras: cameras,
                          sourceArchiveURL: sourceArchiveURL.map { URL(fileURLWithPath: $0.lastPathComponent) })
        try JSONEncoder().encode(stored).write(to: folder.appendingPathComponent("current.json"), options: .atomic)
    }
    static func restore() throws -> Self? {
        let folder = try importFolder(), manifest = folder.appendingPathComponent("current.json")
        guard FileManager.default.fileExists(atPath: manifest.path) else { return nil }
        let data = try Data(contentsOf: manifest)
        guard data.count <= 1_048_576 else { throw SiteError.tooLarge }
        let stored = try JSONDecoder().decode(Self.self, from: data)
        guard stored.modelURL.pathExtension.lowercased() == "usdz" else { throw SiteError.invalidModel }
        let site = Self(name: stored.name, modelURL: folder.appendingPathComponent(stored.modelURL.lastPathComponent), metresPerUnit: stored.metresPerUnit, cameras: stored.cameras,
                        sourceArchiveURL: stored.sourceArchiveURL.map { folder.appendingPathComponent($0.lastPathComponent) })
        guard
              FileManager.default.fileExists(atPath: site.modelURL.path), site.metresPerUnit.isFinite,
              site.metresPerUnit > 0, site.metresPerUnit <= 1000 else { throw SiteError.invalidSite }
        return site
    }
    func writeArchive() throws -> URL {
        let archive: Data
        if let sourceArchiveURL {
            archive = try SpatialFileImport.read(sourceArchiveURL, maximumBytes: SiteArchivePayload.maximumBytes)
            _ = try SiteArchivePayload(data: archive)
        } else {
            archive = try SiteArchivePayload(site: Site(name: name, metresPerModelUnit: Double(metresPerUnit), cameras: cameras),
                                            model: Data(contentsOf: modelURL)).data()
        }
        let directory = try Self.importFolder().appendingPathComponent("Exports").appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let title = String(name.components(separatedBy: CharacterSet(charactersIn: "/:\\")).joined(separator: "-").prefix(80))
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let url = directory.appendingPathComponent(title.isEmpty ? "Room" : title).appendingPathExtension("wendysite")
        try archive.write(to: url, options: .atomic)
        return url
    }

    static func load(_ url: URL) throws -> Self {
        let data = try SpatialFileImport.read(url, maximumBytes: SiteArchivePayload.maximumBytes)
        let payload: SiteArchivePayload?
        let model: Data
        if data.starts(with: Data("WENDYST".utf8)) {
            payload = try SiteArchivePayload(data: data)
            model = payload!.model
        } else {
            payload = nil; model = data
        }
        guard model.count <= SiteArchivePayload.maximumModelBytes, model.starts(with: [0x50, 0x4b, 0x03, 0x04]) else { throw SiteError.invalidModel }
        let folder = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
            .appendingPathComponent("SpatialImports")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let destination = folder.appendingPathComponent(UUID().uuidString).appendingPathExtension("usdz")
        try model.write(to: destination, options: .atomic)
        var archiveURL: URL?
        if payload != nil {
            let saved = destination.deletingPathExtension().appendingPathExtension("wendysite")
            try data.write(to: saved, options: .atomic); archiveURL = saved
        }
        return Self(name: payload?.site.name ?? url.deletingPathExtension().lastPathComponent,
                    modelURL: destination, metresPerUnit: Float(payload?.site.metresPerModelUnit ?? 1),
                    cameras: payload?.site.cameras ?? [], sourceArchiveURL: archiveURL)
    }
}

/// Coordinate reads from Files and other document providers before parsing.
enum SpatialFileImport {
    static func stage(_ url: URL) throws -> URL {
        let data = try read(url, maximumBytes: SiteArchivePayload.maximumBytes)
        let folder = FileManager.default.temporaryDirectory.appendingPathComponent("SpatialInbox").appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let staged = folder.appendingPathComponent(url.lastPathComponent)
        try data.write(to: staged, options: .atomic)
        return staged
    }
    static func discardStaged(_ url: URL) {
        let inbox = FileManager.default.temporaryDirectory.appendingPathComponent("SpatialInbox").standardizedFileURL
        let folder = url.deletingLastPathComponent().standardizedFileURL
        if folder.deletingLastPathComponent() == inbox { try? FileManager.default.removeItem(at: folder) }
    }
    static func read(_ url: URL, maximumBytes: Int) throws -> Data {
        let access = url.startAccessingSecurityScopedResource()
        defer { if access { url.stopAccessingSecurityScopedResource() } }
        let coordinator = NSFileCoordinator()
        var coordinationError: NSError?
        var result: Result<Data, Error>?
        coordinator.coordinate(readingItemAt: url, options: [], error: &coordinationError) { localURL in
            result = Result {
                if let size = try localURL.resourceValues(forKeys: [.fileSizeKey]).fileSize, size > maximumBytes {
                    throw SpatialFailure("This file exceeds the \(maximumBytes / 1024 / 1024) MB import limit.")
                }
                let handle = try FileHandle(forReadingFrom: localURL)
                defer { try? handle.close() }
                var data = Data()
                while data.count <= maximumBytes {
                    let chunk = try handle.read(upToCount: min(1_048_576, maximumBytes + 1 - data.count)) ?? Data()
                    if chunk.isEmpty { return data }
                    data.append(chunk)
                }
                throw SiteError.tooLarge
            }
        }
        if let coordinationError { throw coordinationError }
        guard let result else { throw SpatialFailure("The file provider did not supply the selected file. Try downloading it in Files first.") }
        return try result.get()
    }
}
