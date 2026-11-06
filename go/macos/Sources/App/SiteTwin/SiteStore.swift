import Foundation
import SceneKit

public struct StoredSite: Sendable, Identifiable {
    public var site: Site
    public var modelURL: URL
    public var visualMapURL: URL? = nil
    public var points: [SitePoint] = []
    public var floorTiles: [SitePoint] = []
    public var revision: UInt64 = 0
    public var id: UUID { site.id }
}

/// Site data contains no connection credentials. Import never initiates network
/// access; the host app supplies cameras from its authenticated connection.
public actor SiteStore {
    public static let maximumModelBytes = 128 * 1024 * 1024
    private let directory: URL
    private let endpoint: URL?
    private var remoteRooms: [UUID: RemoteRoom] = [:]
    private var rendered: [UUID: StoredSite] = [:]

    private struct RemoteRoom: Codable {
        var site: Site
        var revision: UInt64
        var model: Data?
        var visualMap: SiteVisualMap?
        var points: [SitePoint]?
        var floorTiles: [SitePoint]?
        var removed: Bool
        var sources: [String: UInt64]?
        var references: [String: [RobotObservedSurface]]?
    }

    public static func remoteStore(session: UInt64) async throws -> SiteStore {
        let endpoint = try await GoClient.shared.openApp("dev.wendy.digital-twin", session: session)
        let cache = FileManager.default.temporaryDirectory.appendingPathComponent("WendyTwin-" + UUID().uuidString)
        return SiteStore(directory: cache, endpoint: endpoint)
    }

    private init(directory: URL, endpoint: URL) { self.directory = directory; self.endpoint = endpoint }

    private func request(_ path: String, method: String = "GET", body: Data? = nil) async throws -> Data {
        guard let endpoint else { throw SiteError.missingSite }
        var request = URLRequest(url: endpoint.appendingPathComponent(path))
        request.httpMethod = method
        request.httpBody = body
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.timeoutInterval = 30
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
            throw WendyError(message: "The Linux digital twin rejected the request. Refresh the room before retrying. " + String(decoding: data.prefix(1024), as: UTF8.self))
        }
        return data
    }

    private func remoteList(removed: Bool) async throws -> [StoredSite] {
        let summaries = try JSONDecoder().decode([RemoteRoom].self, from: await request("v1/rooms"))
        var result: [StoredSite] = []
        for summary in summaries where summary.removed == removed {
            if remoteRooms[summary.site.id]?.revision != summary.revision || rendered[summary.site.id] == nil {
                _ = try await remoteLoad(summary.site.id)
            }
            if let item = rendered[summary.site.id] { result.append(item) }
        }
        return result.sorted { $0.site.name.localizedStandardCompare($1.site.name) == .orderedAscending }
    }

    private func remoteLoad(_ id: UUID) async throws -> StoredSite {
        let previous = remoteRooms[id]
        let suffix = previous == nil ? "" : "/state"
        var room = try JSONDecoder().decode(RemoteRoom.self, from: await request("v1/rooms/" + id.uuidString.lowercased() + suffix))
        if let previous, previous.site.modelRevision == room.site.modelRevision {
            room.model = previous.model; room.visualMap = previous.visualMap
        }
        return try cache(room)
    }

    private func cache(_ room: RemoteRoom) throws -> StoredSite {
        try room.site.validate()
        let folder = directory.appendingPathComponent(room.site.id.uuidString).appendingPathComponent(room.site.modelRevision.uuidString)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let model: URL
        if let bytes = room.model {
            model = folder.appendingPathComponent("model.usdz")
            if !FileManager.default.fileExists(atPath: model.path) { try bytes.write(to: model, options: .atomic) }
            try Self.validateModel(model)
        } else {
            model = folder.appendingPathComponent("model.scn")
            guard FileManager.default.fileExists(atPath: model.path) || SCNScene().write(to: model, options: nil, delegate: nil, progressHandler: nil) else { throw SiteError.invalidModel }
        }
        // A render cache only. Linux owns merging and retaining observations.
        try JSONEncoder().encode(room.points ?? []).write(to: folder.appendingPathComponent("points.json"), options: .atomic)
        var map: URL?
        if let visualMap = room.visualMap {
            map = folder.appendingPathComponent("visual-map.json")
            try JSONEncoder().encode(visualMap).write(to: map!, options: .atomic)
        }
        remoteRooms[room.site.id] = room
        let stored = StoredSite(site: room.site, modelURL: model, visualMapURL: map, points: room.points ?? [], floorTiles: room.floorTiles ?? [], revision: room.revision)
        rendered[room.site.id] = stored
        return stored
    }

    private func putRemote(_ room: RemoteRoom) async throws -> StoredSite {
        let data = try await request("v1/rooms/" + room.site.id.uuidString.lowercased(), method: "PUT", body: JSONEncoder().encode(room))
        return try cache(JSONDecoder().decode(RemoteRoom.self, from: data))
    }

    public func contribute(model: URL, to site: Site, transform: [Double]) async throws {
        try Self.validateModel(model)
        let geometry = try await SiteRobotGeometry.semantic(model, scale: 1)
        let values = geometry.architecture + geometry.furniture
        var points: [SitePoint] = []
        let step = max(1, (values.count / 3 + 49_999) / 50_000)
        for index in stride(from: 0, to: values.count / 3, by: step) {
            points.append(SitePoint(x: values[index*3], y: values[index*3+1], z: values[index*3+2]))
        }
        struct Contribution: Encodable {
            var source: String
            var sequence: UInt64
            var modelRevision: UUID
            var points: [SitePoint]
            var transform: [Double]
        }
        let defaults = UserDefaults.standard
        let node = defaults.string(forKey: "WendyTwinNodeID") ?? UUID().uuidString
        defaults.set(node, forKey: "WendyTwinNodeID")
        let value = Contribution(source: "apple:" + node, sequence: UInt64(Date().timeIntervalSince1970 * 1_000_000),
                                 modelRevision: site.modelRevision, points: points, transform: transform)
        _ = try await request("v1/rooms/" + site.id.uuidString.lowercased() + "/contributions", method: "POST", body: JSONEncoder().encode(value))
        _ = try await remoteLoad(site.id)
    }

    public func migrateLocalCaptures() async throws {
        let legacy = try SiteStore.applicationStore()
        let active = try await remoteList(removed: false)
        let removed = try await remoteList(removed: true)
        let known = Set((active + removed).map(\.id))
        for capture in try await legacy.list() where !known.contains(capture.id) {
            let archive = try await legacy.archive(id: capture.id)
            _ = try await putRemote(RemoteRoom(site: archive.site, revision: 0, model: archive.model, visualMap: archive.visualMap, removed: false))
        }
    }

    func updatePeople(site: Site, detections: [SitePersonDetection]) async throws -> [SitePersonTrack] {
        struct Observation: Encodable { var camera: UUID; var position: SitePoint; var observedAt: Double }
        struct Request: Encodable { var modelRevision: UUID; var observations: [Observation] }
        struct Track: Decodable { var id: UUID; var position: SitePoint; var observedAt: Double }
        let input = Request(modelRevision: site.modelRevision, observations: detections.map {
            Observation(camera: $0.camera, position: SitePoint(x: $0.position.x, y: $0.position.y, z: $0.position.z), observedAt: $0.observedAt.timeIntervalSince1970)
        })
        let data = try await request("v1/rooms/" + site.id.uuidString.lowercased() + "/people", method: "POST", body: JSONEncoder().encode(input))
        return try JSONDecoder().decode([Track].self, from: data).map {
            SitePersonTrack(id: $0.id, position: SIMD3($0.position.x, $0.position.y, $0.position.z), observedAt: Date(timeIntervalSince1970: $0.observedAt))
        }
    }

    public func createSpace(name: String) async throws -> StoredSite {
        try await putRemote(RemoteRoom(site: Site(name: name), revision: 0, removed: false))
    }


    public init(directory: URL) { self.directory = directory; self.endpoint = nil }

    public static func applicationStore() throws -> SiteStore {
        let base = try FileManager.default.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        return SiteStore(directory: base.appendingPathComponent("WendySites", isDirectory: true))
    }

    public func list() async throws -> [StoredSite] {
        if endpoint != nil { return try await remoteList(removed: false) }
        guard FileManager.default.fileExists(atPath: directory.path) else { return [] }
        return try FileManager.default.contentsOfDirectory(
            at: directory,
            includingPropertiesForKeys: nil
        )
        .filter { UUID(uuidString: $0.lastPathComponent) != nil }
        .map { try read(id: UUID(uuidString: $0.lastPathComponent)!, folder: $0) }
        .sorted { $0.site.name.localizedStandardCompare($1.site.name) == .orderedAscending }
    }

    public func load(id: UUID) async throws -> StoredSite {
        if endpoint != nil { return try await remoteLoad(id) }
        return try read(id: id, folder: folder(id))
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
    public func remove(id: UUID) async throws {
        if endpoint != nil {
            guard var room = remoteRooms[id] else { throw SiteError.missingSite }
            room.removed = true; _ = try await putRemote(room); return
        }
        let destination = removedDirectory.appendingPathComponent(id.uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: removedDirectory, withIntermediateDirectories: true)
        try FileManager.default.moveItem(at: folder(id), to: destination)
    }

    public func removed() async throws -> [StoredSite] {
        if endpoint != nil { return try await remoteList(removed: true) }
        guard FileManager.default.fileExists(atPath: removedDirectory.path) else { return [] }
        return try FileManager.default.contentsOfDirectory(at: removedDirectory, includingPropertiesForKeys: nil)
            .compactMap { url -> StoredSite? in
                guard let id = UUID(uuidString: url.lastPathComponent) else { return nil }
                return try read(id: id, folder: url)
            }
            .sorted { $0.site.name.localizedStandardCompare($1.site.name) == .orderedAscending }
    }

    public func restore(id: UUID) async throws -> StoredSite {
        if endpoint != nil {
            guard var room = remoteRooms[id] else { throw SiteError.missingSite }
            room.removed = false; return try await putRemote(room)
        }
        let source = removedDirectory.appendingPathComponent(id.uuidString, isDirectory: true)
        _ = try read(id: id, folder: source)
        try FileManager.default.moveItem(at: source, to: folder(id))
        return try await load(id: id)
    }

    private var removedDirectory: URL {
        directory.appendingPathComponent("Recently Removed", isDirectory: true)
    }

    public func create(name: String, model: URL) async throws -> StoredSite {
        if endpoint != nil {
            try Self.validateModel(model)
            return try await putRemote(RemoteRoom(site: Site(name: name), revision: 0, model: Data(contentsOf: model), removed: false))
        }
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
            try await save(site)
            return try await load(id: site.id)
        } catch {
            try? FileManager.default.removeItem(at: destination)
            throw error
        }
    }

    public func save(_ site: Site) async throws {
        if endpoint != nil {
            guard var room = remoteRooms[site.id] else { throw SiteError.missingSite }
            room.site = site; _ = try await putRemote(room); return
        }
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

    public func archive(id: UUID) async throws -> SiteArchive {
        if endpoint != nil {
            _ = try await remoteLoad(id)
            guard let room = remoteRooms[id], let model = room.model else { throw WendyError(message: "This robot room has no USDZ capture to export.") }
            return try SiteArchive(site: room.site, model: model, visualMap: room.visualMap)
        }
        let saved = try await load(id: id)
        let model = try Data(contentsOf: saved.modelURL)
        let map = try saved.visualMapURL.map { try SiteVisualMap.read($0, revision: saved.site.modelRevision, model: model) }
        return try SiteArchive(site: saved.site, model: model, visualMap: map)
    }

    nonisolated static func shareFilename(_ name: String) -> String {
        let separators = CharacterSet(charactersIn: "/\\:").union(.controlCharacters)
        let clean = name.components(separatedBy: separators).joined(separator: "-")
            .trimmingCharacters(in: .whitespacesAndNewlines.union(CharacterSet(charactersIn: ".")))
        let stem = String(String.UnicodeScalarView(clean.unicodeScalars.prefix(40)))
        return (stem.isEmpty ? "Digital Twin" : stem) + ".wendysite"
    }

    /// Give each transfer its own file so concurrent shares cannot overwrite it.
    /// Keep it in temporary storage while the system copies it for AirDrop.
    func shareArchive(id: UUID) async throws -> URL {
        let archive = try await archive(id: id)
        try Task.checkCancellation()
        let folder = FileManager.default.temporaryDirectory
            .appendingPathComponent("WendyShare-" + UUID().uuidString, isDirectory: true)
        do {
            try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
            let file = folder.appendingPathComponent(Self.shareFilename(archive.site.name))
            try archive.data().write(to: file, options: .atomic)
            try Task.checkCancellation()
            return file
        } catch {
            try? FileManager.default.removeItem(at: folder)
            throw error
        }
    }

    /// Imports a copy without overwriting a local site. Placements remain in the
    /// same model revision and gateway identities still require authentication.
    public func importArchive(_ archive: SiteArchive) async throws -> StoredSite {
        if endpoint != nil {
            var site = archive.site; site.id = UUID()
            return try await putRemote(RemoteRoom(site: site, revision: 0, model: archive.model, visualMap: archive.visualMap, removed: false))
        }
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
            try await save(site)
            return try await load(id: site.id)
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
