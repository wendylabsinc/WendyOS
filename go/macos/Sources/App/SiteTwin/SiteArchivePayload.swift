import Foundation

/// The same bounded archive framing is used by every Apple client.
struct SiteArchivePayload: Sendable {
    static let maximumModelBytes = 128 * 1024 * 1024
    static let maximumMapBytes = 12 * 1024 * 1024
    static let maximumBytes = maximumModelBytes + maximumMapBytes + 1_048_576 + 20
    let site: Site
    let model: Data
    let visualMap: Data?

    init(site: Site, model: Data, visualMap: Data? = nil) throws {
        try site.validate()
        guard model.count <= Self.maximumModelBytes, model.starts(with: [0x50, 0x4b, 0x03, 0x04]) else { throw SiteError.invalidModel }
        if let visualMap { guard !visualMap.isEmpty, visualMap.count <= Self.maximumMapBytes else { throw SiteError.invalidSite } }
        self.site = site; self.model = model; self.visualMap = visualMap
    }

    init(data: Data) throws {
        guard (16...Self.maximumBytes).contains(data.count) else { throw SiteError.invalidSite }
        func length(_ offset: Int) -> Int { data.dropFirst(offset).prefix(4).reduce(0) { ($0 << 8) | Int($1) } }
        let manifestSize = length(8)
        guard (1...1_048_576).contains(manifestSize) else { throw SiteError.invalidSite }
        let header = String(decoding: data.prefix(8), as: UTF8.self)
        if header == "WENDYST1" {
            guard 12 + manifestSize < data.count else { throw SiteError.invalidSite }
            try self.init(site: JSONDecoder().decode(Site.self, from: data.subdata(in: 12..<(12 + manifestSize))),
                          model: Data(data.dropFirst(12 + manifestSize)))
        } else if header == "WENDYST2" {
            guard data.count >= 20 else { throw SiteError.invalidSite }
            let modelSize = length(12), mapSize = length(16)
            guard (1...Self.maximumModelBytes).contains(modelSize), (1...Self.maximumMapBytes).contains(mapSize),
                  20 + manifestSize + modelSize + mapSize == data.count else { throw SiteError.invalidSite }
            let modelStart = 20 + manifestSize, mapStart = modelStart + modelSize
            try self.init(site: JSONDecoder().decode(Site.self, from: data.subdata(in: 20..<modelStart)),
                          model: data.subdata(in: modelStart..<mapStart), visualMap: data.subdata(in: mapStart..<data.count))
        } else { throw SiteError.unsupportedVersion }
    }

    func data() throws -> Data {
        let manifest = try JSONEncoder().encode(site)
        guard manifest.count <= 1_048_576 else { throw SiteError.invalidSite }
        var result = Data((visualMap == nil ? "WENDYST1" : "WENDYST2").utf8)
        func appendLength(_ count: Int) {
            var value = UInt32(count).bigEndian
            withUnsafeBytes(of: &value) { result.append(contentsOf: $0) }
        }
        appendLength(manifest.count)
        if let visualMap { appendLength(model.count); appendLength(visualMap.count) }
        result.append(manifest); result.append(model)
        if let visualMap { result.append(visualMap) }
        return result
    }
}
