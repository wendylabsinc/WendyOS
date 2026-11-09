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
    public static let maximumBytes = SiteArchivePayload.maximumBytes
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
        let payload = try SiteArchivePayload(data: data)
        try self.init(site: payload.site, model: payload.model,
                      visualMap: try payload.visualMap.map { try JSONDecoder().decode(SiteVisualMap.self, from: $0) })
    }

    public func data() throws -> Data {
        try SiteArchivePayload(site: site, model: model,
                               visualMap: try visualMap.map { try JSONEncoder().encode($0) }).data()
    }
}
