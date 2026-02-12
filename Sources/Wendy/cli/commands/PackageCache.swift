import Crypto
import Foundation

/// Caches Swift package analysis results to avoid repeated calls to `swift package show-executables`
/// and `swift package show-dependencies` when Package.swift hasn't changed.
struct PackageCache: Sendable {
    struct CachedPackageInfo: Codable, Sendable {
        let packageSwiftHash: String
        let packageIdentity: String
        let products: [Serialization.Product]
        let hasContainerPlugin: Bool
    }

    let projectPath: URL

    var cacheDirectory: URL {
        projectPath.appendingPathComponent(".wendy/cache")
    }

    var cacheFile: URL {
        cacheDirectory.appendingPathComponent("package-info.json")
    }

    var packageSwiftFile: URL {
        projectPath.appendingPathComponent("Package.swift")
    }

    /// Compute SHA256 hash of Package.swift contents
    func computePackageSwiftHash() throws -> String {
        let data = try Data(contentsOf: packageSwiftFile)
        let hash = SHA256.hash(data: data)
        return hash.map { String(format: "%02x", $0) }.joined()
    }

    /// Read cached package info from disk
    func read() -> CachedPackageInfo? {
        guard let data = try? Data(contentsOf: cacheFile) else {
            return nil
        }
        return try? JSONDecoder().decode(CachedPackageInfo.self, from: data)
    }

    /// Write package info to cache
    func write(_ info: CachedPackageInfo) throws {
        if !FileManager.default.fileExists(atPath: cacheDirectory.path) {
            try FileManager.default.createDirectory(at: cacheDirectory, withIntermediateDirectories: true)
        }

        let data = try JSONEncoder().encode(info)
        try data.write(to: cacheFile, options: .atomic)
    }

    /// Delete the cache file
    func invalidate() {
        try? FileManager.default.removeItem(at: cacheFile)
    }

    /// Returns cached info only if the Package.swift hash matches
    func getValidCache() -> CachedPackageInfo? {
        guard let cached = read() else {
            return nil
        }

        guard let currentHash = try? computePackageSwiftHash() else {
            return nil
        }

        if cached.packageSwiftHash == currentHash {
            return cached
        }
        
        return nil
    }
}
