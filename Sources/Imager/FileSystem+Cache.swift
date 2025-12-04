import Foundation

extension FileManager {
    public func cacheDirectory(_ type: CacheType) throws -> URL {
        try url(for: .cachesDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
            .appendingPathComponent(".wendy/\(type.rawValue)")
    }
}

public enum CacheType: String, Sendable {
    case images
    case agents
}
