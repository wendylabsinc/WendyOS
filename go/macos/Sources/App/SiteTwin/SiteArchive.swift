import Foundation
import CoreTransferable
import UniformTypeIdentifiers

/// Prepare the archive only when the system requests it for sharing.
struct SiteShare: Transferable, Sendable {
    let store: SiteStore
    let id: UUID
    let name: String
    let onFailure: @MainActor @Sendable (any Error) -> Void

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(exportedContentType: .data) { item in
            do {
                return SentTransferredFile(try await item.store.shareArchive(id: item.id))
            } catch {
                await item.onFailure(error)
                throw error
            }
        }
        .suggestedFileName { SiteStore.shareFilename($0.name) }
    }
}

/// V1 stores a manifest and USDZ. V2 adds a bounded visual reference map, tied
/// to the exact model bytes and revision. Neither format contains credentials.
public struct SiteArchive: Sendable {
    private static let magic = Data("WENDYST1".utf8)
    private static let mapMagic = Data("WENDYST2".utf8)
    public static let maximumBytes = SiteStore.maximumModelBytes + SiteVisualMap.maximumBytes + 1_048_576 + 20
    public let site: Site
    public let model: Data
    public let visualMap: SiteVisualMap?

    public init(site: Site, model: Data, visualMap: SiteVisualMap? = nil) throws {
        try site.validate()
        guard model.count <= SiteStore.maximumModelBytes, model.starts(with: [0x50,0x4b,0x03,0x04]) else { throw SiteError.invalidModel }
        try visualMap?.validate(modelRevision: site.modelRevision, model: model)
        self.site = site; self.model = model; self.visualMap = visualMap
    }

    public init(data: Data) throws {
        guard data.count <= Self.maximumBytes, data.count >= 16 else { throw SiteError.invalidSite }
        let header = Data(data.prefix(8))
        func length(_ offset: Int) -> Int { data.dropFirst(offset).prefix(4).reduce(0) { ($0 << 8) | Int($1) } }
        let manifestSize = length(8)
        guard manifestSize > 0, manifestSize <= 1_048_576 else { throw SiteError.invalidSite }
        if header == Self.magic {
            guard 12+manifestSize < data.count else { throw SiteError.invalidSite }
            try self.init(site: JSONDecoder().decode(Site.self, from: data.subdata(in: 12..<(12+manifestSize))), model: Data(data.dropFirst(12+manifestSize)))
        } else if header == Self.mapMagic {
            guard data.count >= 20 else { throw SiteError.invalidSite }
            let modelSize = length(12), mapSize = length(16)
            guard modelSize > 0, modelSize <= SiteStore.maximumModelBytes, mapSize > 0, mapSize <= SiteVisualMap.maximumBytes,
                  20+manifestSize+modelSize+mapSize == data.count else { throw SiteError.invalidSite }
            let modelStart = 20+manifestSize, mapStart = modelStart+modelSize
            try self.init(site: JSONDecoder().decode(Site.self, from: data.subdata(in: 20..<modelStart)),
                model: data.subdata(in: modelStart..<mapStart),
                visualMap: JSONDecoder().decode(SiteVisualMap.self, from: data.subdata(in: mapStart..<data.count)))
        } else { throw SiteError.invalidSite }
    }

    public func data() throws -> Data {
        let manifest = try JSONEncoder().encode(site)
        guard manifest.count <= 1_048_576 else { throw SiteError.invalidSite }
        func appendLength(_ count: Int, to result: inout Data) {
            let value = UInt32(count)
            result.append(contentsOf: [UInt8(value >> 24), UInt8((value >> 16)&255), UInt8((value >> 8)&255), UInt8(value&255)])
        }
        let map = try visualMap.map { try JSONEncoder().encode($0) }
        guard (map?.count ?? 0) <= SiteVisualMap.maximumBytes else { throw SiteError.tooLarge }
        var result = map == nil ? Self.magic : Self.mapMagic
        appendLength(manifest.count, to: &result)
        if let map { appendLength(model.count, to: &result); appendLength(map.count, to: &result) }
        result.append(manifest); result.append(model)
        if let map { result.append(map) }
        return result
    }
}
