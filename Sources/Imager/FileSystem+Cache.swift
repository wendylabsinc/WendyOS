import Foundation

extension FileManager {
    /// Returns the cache directory for the given cache type, migrating from legacy `.wendy` if needed.
    public func cacheDirectory(_ type: CacheType) throws -> URL {
        let cachesRoot = try url(
            for: .cachesDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        let newCacheDir = cachesRoot.appendingPathComponent("sh.wendy/\(type.rawValue)")
        let legacyCacheDir = cachesRoot.appendingPathComponent(".wendy/\(type.rawValue)")

        // Migrate from legacy .wendy to sh.wendy if needed
        migrateIfNeeded(from: legacyCacheDir, to: newCacheDir)

        // Ensure the new directory exists
        if !fileExists(atPath: newCacheDir.path) {
            try createDirectory(at: newCacheDir, withIntermediateDirectories: true)
        }

        return newCacheDir
    }

    /// Migrates cache from legacy location to new location if the legacy exists and new doesn't.
    private func migrateIfNeeded(from legacyDir: URL, to newDir: URL) {
        // Only migrate if legacy exists and new doesn't
        guard fileExists(atPath: legacyDir.path) else { return }
        guard !fileExists(atPath: newDir.path) else { return }

        // Ensure parent directory exists
        let newParent = newDir.deletingLastPathComponent()
        if !fileExists(atPath: newParent.path) {
            try? createDirectory(at: newParent, withIntermediateDirectories: true)
        }

        // Move the legacy directory to the new location
        do {
            try moveItem(at: legacyDir, to: newDir)
        } catch {
            // If move fails, try copying instead (cross-volume moves can fail)
            try? copyItem(at: legacyDir, to: newDir)
            try? removeItem(at: legacyDir)
        }

        // Clean up empty legacy parent directory
        let legacyParent = legacyDir.deletingLastPathComponent()
        if let contents = try? contentsOfDirectory(atPath: legacyParent.path), contents.isEmpty {
            try? removeItem(at: legacyParent)
        }
    }
}

public enum CacheType: String, Sendable {
    case images
    case agents
}
