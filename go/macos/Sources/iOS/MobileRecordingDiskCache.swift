import Foundation
import CryptoKit

// Re-creatable data only. Account/org and device identity are hashed into every
// key. Playback media is separately checksum-verified and bounded in Go.
actor RecordingDiskCache {
    static let shared = RecordingDiskCache()
    let directory: URL
    let byteLimit: Int
    let entryLimit: Int
    init(directory: URL = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("WendyGallery-v1"), byteLimit: Int = 64 * 1024 * 1024, entryLimit: Int = 160) {
        self.directory = directory; self.byteLimit = byteLimit; self.entryLimit = entryLimit
    }
    private func url(_ key: String) -> URL {
        directory.appendingPathComponent(SHA256.hash(data: Data(key.utf8)).map { String(format: "%02x", $0) }.joined())
    }
    func read(_ key: String) -> Data? {
        let file = url(key)
        guard let attrs = try? FileManager.default.attributesOfItem(atPath: file.path),
              let size = attrs[.size] as? Int, size <= byteLimit,
              let modified = attrs[.modificationDate] as? Date, Date().timeIntervalSince(modified) < 7 * 86400 else { return nil }
        return try? Data(contentsOf: file)
    }
    func readIndex(_ key: String) -> SavedRecordingIndex? {
        guard let data = read(key) else { return nil }
        return try? JSONDecoder().decode(SavedRecordingIndex.self, from: data)
    }
    func writeIndex(_ index: SavedRecordingIndex, key: String) {
        guard let data = try? JSONEncoder().encode(index) else { return }
        write(data, key: key)
    }
    func write(_ data: Data, key: String) {
        guard data.count <= byteLimit else { return }
        let fm = FileManager.default
        do {
            try fm.createDirectory(at: directory, withIntermediateDirectories: true)
            try data.write(to: url(key), options: .atomic)
            let files = try fm.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.contentModificationDateKey, .fileSizeKey])
            let sorted = files.compactMap { file -> (URL, Date, Int)? in
                guard let values = try? file.resourceValues(forKeys: [.contentModificationDateKey, .fileSizeKey]) else { return nil }
                return (file, values.contentModificationDate ?? .distantPast, values.fileSize ?? 0)
            }.sorted { $0.1 > $1.1 }
            var total = 0
            for (index, item) in sorted.enumerated() {
                total += item.2
                if index >= entryLimit || total > byteLimit { try? fm.removeItem(at: item.0) }
            }
        } catch { /* Cache failures must not prevent browsing. */ }
    }
}

struct SavedRecordingIndex: Codable {
    let model: String?
    let episodes: [MobileRecordingEpisode]
}
