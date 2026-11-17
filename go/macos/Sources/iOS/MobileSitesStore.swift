import Foundation
import CryptoKit
import SwiftUI
@preconcurrency import SceneKit

struct MobileSiteAsset: Codable, Identifiable, Equatable {
    let id: String
    let name: String
    let robot: Bool
}

struct MobileSiteModelLink: Codable, Identifiable {
    let id: UUID
    var name: String
    var covers: String = "Whole site"
}

struct MobileSiteRecord: Codable, Identifiable {
    var id = UUID()
    let name: String
    var assets: [MobileSiteAsset]
    var models: [MobileSiteModelLink] = []
    var defaultModel: UUID?
    var hideSetup = false
    var assetSummary: String {
        let robots = assets.filter(\.robot).count
        return "\(assets.count - robots) devices · \(robots) robots"
    }
}

struct MobileSiteEvent: Codable, Identifiable {
    var id = UUID()
    let siteID: UUID
    let text: String
    var date = Date()
}

private struct MobileSitesDocument: Codable {
    var version = 1
    var sites: [MobileSiteRecord] = []
    var events: [MobileSiteEvent] = []
    var selected: UUID?
}

// A Cloud site's identity is not a saved 3D capture's identity. Keep a separate
// site record linking N models; never reinterpret legacy captures as Cloud sites.
@MainActor final class MobileSitesStore: ObservableObject {
    @Published private(set) var sites: [MobileSiteRecord] = []
    @Published private(set) var events: [MobileSiteEvent] = []
    @Published private(set) var selected: UUID?
    @Published private(set) var loadError: String?
    let models: SiteStore
    private let directory: URL
    private let importer: MobileSiteModelImporter
    var current: MobileSiteRecord? { sites.first { $0.id == selected } ?? sites.first }

    static func scope(account: String, organization: String) -> String {
        let data = try! JSONEncoder().encode([account, organization])
        return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    init(account: String, organization: String, base: URL? = nil) {
        let root = base ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("MobileSites", isDirectory: true)
        directory = root.appendingPathComponent(Self.scope(account: account, organization: organization), isDirectory: true)
        models = SiteStore(directory: directory.appendingPathComponent("Models", isDirectory: true))
        importer = MobileSiteModelImporter(store: models)
        let file = directory.appendingPathComponent("sites.json")
        guard FileManager.default.fileExists(atPath: file.path) else { return }
        do {
            let document = try JSONDecoder().decode(MobileSitesDocument.self, from: Data(contentsOf: file))
            guard document.version == 1, Set(document.sites.map(\.id)).count == document.sites.count else { throw SiteError.invalidSite }
            sites = document.sites; events = document.events; selected = document.selected
        } catch { loadError = "Saved sites could not be read. Your files have not been replaced. " + error.localizedDescription }
    }

    @discardableResult func create(name: String, assets: [MobileSiteAsset]) throws -> UUID {
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !name.isEmpty, name.count <= 200, Set(assets.map(\.id)).count == assets.count else { throw SiteError.invalidSite }
        let site = MobileSiteRecord(name: name, assets: assets)
        try commit(sites: sites + [site], events: events + [MobileSiteEvent(siteID: site.id, text: "\(name) created")], selected: site.id)
        return site.id
    }
    func setAssets(_ assets: [MobileSiteAsset], siteID: UUID) throws {
        guard let index = sites.firstIndex(where: { $0.id == siteID }) else { throw SiteError.missingSite }
        guard Set(assets.map(\.id)).count == assets.count else { throw SiteError.invalidSite }
        guard sites[index].assets != assets else { return }
        var next = sites; next[index].assets = assets
        try commit(sites: next, events: events + [MobileSiteEvent(siteID: siteID, text: "Devices and robots updated for \(next[index].name)")], selected: selected)
    }
    func select(_ id: UUID) throws {
        guard sites.contains(where: { $0.id == id }) else { throw SiteError.missingSite }
        try commit(sites: sites, events: events, selected: id)
    }
    func setSetupHidden(_ hidden: Bool, siteID: UUID) throws {
        guard let index = sites.firstIndex(where: { $0.id == siteID }) else { throw SiteError.missingSite }
        var next = sites; next[index].hideSetup = hidden
        try commit(sites: next, events: events, selected: selected)
    }
    func setDefault(modelID: UUID, siteID: UUID) throws {
        guard let index = sites.firstIndex(where: { $0.id == siteID }), sites[index].models.contains(where: { $0.id == modelID }) else { throw SiteError.missingSite }
        var next = sites; next[index].defaultModel = modelID
        try commit(sites: next, events: events, selected: selected)
    }
    func renameModel(_ modelID: UUID, siteID: UUID, name: String) async throws {
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !name.isEmpty, name.count <= 200 else { throw WendyError(message: "Use a model name between 1 and 200 characters.") }
        guard loadError == nil, sites.contains(where: { $0.id == siteID && $0.models.contains(where: { $0.id == modelID }) }) else { throw SiteError.missingSite }
        let previous = try await models.load(id: modelID)
        var renamed = previous.site
        renamed.name = name
        try await models.save(renamed)
        do {
            guard let index = sites.firstIndex(where: { $0.id == siteID }), let modelIndex = sites[index].models.firstIndex(where: { $0.id == modelID }) else { throw SiteError.missingSite }
            var next = sites
            next[index].models[modelIndex].name = name
            try commit(sites: next, events: events + [MobileSiteEvent(siteID: siteID, text: "3D model renamed to \(name)")], selected: selected)
        } catch {
            try await models.save(previous.site)
            throw error
        }
    }
    func addModel(from url: URL, to siteID: UUID) async throws {
        guard loadError == nil, sites.contains(where: { $0.id == siteID }) else { throw SiteError.missingSite }
        let saved = try await importer.importFile(url)
        do {
            try Task.checkCancellation()
            guard let index = sites.firstIndex(where: { $0.id == siteID }) else { throw SiteError.missingSite }
            var next = sites
            next[index].models.append(MobileSiteModelLink(id: saved.id, name: saved.site.name))
            if next[index].defaultModel == nil { next[index].defaultModel = saved.id }
            try commit(sites: next, events: events + [MobileSiteEvent(siteID: siteID, text: "\(saved.site.name) linked to \(next[index].name)")], selected: selected)
        } catch {
            try? await models.remove(id: saved.id)
            throw error
        }
    }
    func saveCameraModel(_ value: Site, siteID: UUID, event: String) async throws {
        guard loadError == nil, sites.contains(where: { $0.id == siteID && $0.models.contains(where: { $0.id == value.id }) }) else { throw SiteError.missingSite }
        let previous = try await models.load(id: value.id)
        guard previous.site.modelRevision == value.modelRevision else { throw SiteError.invalidSite }
        try await models.save(value)
        do {
            try commit(sites: sites, events: events + [MobileSiteEvent(siteID: siteID, text: event)], selected: selected)
        } catch {
            try await models.save(previous.site)
            throw error
        }
    }
    private func commit(sites: [MobileSiteRecord], events: [MobileSiteEvent], selected: UUID?) throws {
        guard loadError == nil else { throw SiteError.invalidSite }
        let bounded = Array(events.suffix(1000))
        let document = MobileSitesDocument(sites: sites, events: bounded, selected: selected)
        let data = try JSONEncoder().encode(document)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        try data.write(to: directory.appendingPathComponent("sites.json"), options: .atomic)
        self.sites = sites; self.events = bounded; self.selected = selected
    }
}

// File decoding/SceneKit validation runs away from the main actor. Preserve
// archive placements and visual reference maps, but never connect to an import.
private actor MobileSiteModelImporter {
    let store: SiteStore
    init(store: SiteStore) { self.store = store }
    func importFile(_ url: URL) async throws -> StoredSite {
        let access = url.startAccessingSecurityScopedResource()
        defer { if access { url.stopAccessingSecurityScopedResource() } }
        try Task.checkCancellation()
        if url.pathExtension.lowercased() == "wendysite" {
            let values = try url.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey, .fileSizeKey])
            guard values.isRegularFile == true, values.isSymbolicLink != true,
                  let size = values.fileSize, size <= SiteArchive.maximumBytes else { throw SiteError.invalidSite }
            let archive = try SiteArchive(data: Data(contentsOf: url))
            let temp = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".usdz")
            defer { try? FileManager.default.removeItem(at: temp) }
            try archive.model.write(to: temp)
            try validateScene(temp)
            try Task.checkCancellation()
            return try await store.importArchive(archive)
        }
        try SiteStore.validateModel(url)
        try validateScene(url)
        try Task.checkCancellation()
        return try await store.create(name: url.deletingPathExtension().lastPathComponent, model: url)
    }
    private func validateScene(_ url: URL) throws {
        _ = try SCNScene(url: url, options: [.checkConsistency: true, .convertToYUp: true, .convertUnitsToMeters: 1.0])
    }
}
