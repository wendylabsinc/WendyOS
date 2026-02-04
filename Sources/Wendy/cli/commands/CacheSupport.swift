import ArgumentParser
import Foundation
import Imager

// MARK: - Cache utilities

enum CachedImageStatus: String, Codable {
    case ready
    case incomplete
    case empty

    var displayValue: String {
        switch self {
        case .ready:
            return "Ready"
        case .incomplete:
            return "Incomplete"
        case .empty:
            return "Empty"
        }
    }
}

struct CachedImageEntry: Codable {
    let device: String
    let version: String?
    let cachedAt: Date?
    let imagePath: String?
    let sizeBytes: Int64
    let status: CachedImageStatus
}

func primaryCacheDirectory(fileManager: FileManager = .default) -> URL {
    let fallback =
        fileManager.homeDirectoryForCurrentUser
        .appendingPathComponent("sh.wendy/cache/images")
    return (try? fileManager.cacheDirectory(.images)) ?? fallback
}

func cacheDirectories(fileManager: FileManager = .default) -> [URL] {
    let fallback =
        fileManager.homeDirectoryForCurrentUser
        .appendingPathComponent("sh.wendy/cache/images")
    let systemCache = (try? fileManager.cacheDirectory(.images)) ?? fallback

    // Also check legacy locations for migration
    let legacyLocations = [
        fileManager.homeDirectoryForCurrentUser.appendingPathComponent(".wendy/cache/images")
    ]

    var dirs = [systemCache]
    for legacy in legacyLocations {
        if legacy.path != systemCache.path && fileManager.fileExists(atPath: legacy.path) {
            dirs.append(legacy)
        }
    }

    return dirs
}

func listCachedImages(fileManager: FileManager = .default) throws -> [CachedImageEntry] {
    let roots = cacheDirectories(fileManager: fileManager)
    var entriesByDevice: [String: CachedImageEntry] = [:]

    for root in roots {
        guard fileManager.fileExists(atPath: root.path) else { continue }

        let contents: [URL]
        do {
            contents = try fileManager.contentsOfDirectory(
                at: root,
                includingPropertiesForKeys: [.isDirectoryKey],
                options: [.skipsHiddenFiles]
            )
        } catch {
            throw ValidationError(
                "Failed to read cache directory \(root.path): "
                    + error.localizedDescription
            )
        }

        for url in contents {
            let values = try? url.resourceValues(forKeys: [.isDirectoryKey])
            guard values?.isDirectory == true else { continue }

            let device = url.lastPathComponent
            if entriesByDevice[device] != nil {
                continue
            }

            let (version, timestamp) = readCacheMetadata(at: url)
            let imageURL = findImageFile(in: url, fileManager: fileManager)
            let size = directorySize(of: url, fileManager: fileManager)

            let status: CachedImageStatus
            if imageURL != nil {
                status = .ready
            } else if size > 0 {
                status = .incomplete
            } else {
                status = .empty
            }

            entriesByDevice[device] = CachedImageEntry(
                device: device,
                version: version,
                cachedAt: timestamp,
                imagePath: imageURL?.path,
                sizeBytes: size,
                status: status
            )
        }
    }

    return entriesByDevice.values.sorted { $0.device < $1.device }
}

func cachedImageDownloadDate(
    deviceName: String,
    nightly: Bool,
    fileManager: FileManager = .default
) -> Date? {
    let roots = cacheDirectories(fileManager: fileManager)
    let versionFolder = nightly ? "nightly" : "stable"
    var newest: Date? = nil

    for root in roots {
        let deviceRoot = root.appendingPathComponent(deviceName)
        let candidates = [
            deviceRoot.appendingPathComponent(versionFolder),
            deviceRoot,
        ]

        for candidate in candidates {
            if let date = latestImageFileDate(in: candidate, fileManager: fileManager) {
                if newest == nil || date > newest! {
                    newest = date
                }
            }
        }
    }

    return newest
}

private func latestImageFileDate(
    in directory: URL,
    fileManager: FileManager = .default
) -> Date? {
    guard fileManager.fileExists(atPath: directory.path) else { return nil }
    guard
        let enumerator = fileManager.enumerator(
            at: directory,
            includingPropertiesForKeys: [
                .isRegularFileKey,
                .creationDateKey,
                .contentModificationDateKey,
            ],
            options: [.skipsHiddenFiles]
        )
    else { return nil }

    var newest: Date? = nil
    for case let fileURL as URL in enumerator {
        guard fileURL.pathExtension.lowercased() == "img" else { continue }

        let values = try? fileURL.resourceValues(
            forKeys: [.creationDateKey, .contentModificationDateKey]
        )
        let date = values?.creationDate ?? values?.contentModificationDate
        if let date {
            if newest == nil || date > newest! {
                newest = date
            }
        }
    }

    return newest
}

private func readCacheMetadata(at url: URL) -> (String?, Date?) {
    let metadataURL = url.appendingPathComponent("version.json")

    guard let data = try? Data(contentsOf: metadataURL) else {
        return (nil, nil)
    }

    struct CacheVersionMetadata: Codable {
        let version: String
        let timestamp: Date
    }

    let decoder = JSONDecoder()
    decoder.dateDecodingStrategy = .iso8601
    guard let metadata = try? decoder.decode(CacheVersionMetadata.self, from: data) else {
        return (nil, nil)
    }

    return (metadata.version, metadata.timestamp)
}

private func directorySize(of url: URL, fileManager: FileManager = .default) -> Int64 {
    guard
        let enumerator = fileManager.enumerator(
            at: url,
            includingPropertiesForKeys: [
                .isRegularFileKey,
                .totalFileAllocatedSizeKey,
                .fileSizeKey,
            ],
            options: [.skipsHiddenFiles]
        )
    else { return 0 }

    var total: Int64 = 0
    for case let fileURL as URL in enumerator {
        guard
            let values = try? fileURL.resourceValues(
                forKeys: [.isRegularFileKey, .totalFileAllocatedSizeKey, .fileSizeKey]
            ),
            values.isRegularFile == true
        else { continue }

        if let allocated = values.totalFileAllocatedSize {
            total += Int64(allocated)
        } else if let size = values.fileSize {
            total += Int64(size)
        }
    }

    return total
}

private func findImageFile(in url: URL, fileManager: FileManager = .default) -> URL? {
    guard
        let enumerator = fileManager.enumerator(
            at: url,
            includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]
        )
    else { return nil }

    for case let fileURL as URL in enumerator {
        guard
            let values = try? fileURL.resourceValues(forKeys: [.isRegularFileKey]),
            values.isRegularFile == true
        else { continue }

        if fileURL.pathExtension.lowercased() == "img" {
            return fileURL
        }
    }

    return nil
}
