import ArgumentParser
import Foundation
import Logging
import Noora

struct CacheCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "cache",
        abstract: "Inspect cached WendyOS images.",
        subcommands: [
            ListCommand.self
        ],
        defaultSubcommand: ListCommand.self
    )

    struct ListCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list",
            abstract: "List cached WendyOS images."
        )

        fileprivate struct JSONOutput: Codable {
            let images: [CachedImageEntry]
        }

        @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
        var json: Bool = false

        func run() async throws {
            let cachedImages = try listCachedImages()

            if json {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.prettyPrinted]
                encoder.dateEncodingStrategy = .iso8601
                let data = try encoder.encode(JSONOutput(images: cachedImages))
                guard let jsonString = String(data: data, encoding: .utf8) else { return }
                print(jsonString)
                return
            }

            guard !cachedImages.isEmpty else {
                print("No cached WendyOS images found.")
                return
            }

            let dateFormatter = DateFormatter()
            dateFormatter.dateStyle = .medium
            dateFormatter.timeStyle = .short

            Noora().table(
                headers: [
                    "Device",
                    "Version",
                    "Size",
                    "Cached",
                    "Status",
                ],
                rows: cachedImages.map { entry in
                    [
                        entry.device,
                        entry.version ?? "—",
                        ByteCountFormatter.string(
                            fromByteCount: entry.sizeBytes,
                            countStyle: .file
                        ),
                        entry.cachedAt.map { dateFormatter.string(from: $0) } ?? "—",
                        entry.status.displayValue,
                    ]
                }
            )

            if cachedImages.contains(where: { $0.status != .ready }) {
                print(
                    "\nSome cache entries look incomplete. Remove the corresponding folders under \(cacheDirectory().path) if you need to reclaim space."
                )
            }
        }
    }
}
// MARK: - Cache listing

private enum CachedImageStatus: String, Codable {
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

private struct CachedImageEntry: Codable {
    let device: String
    let version: String?
    let cachedAt: Date?
    let imagePath: String?
    let sizeBytes: Int64
    let status: CachedImageStatus
}

private func cacheDirectory(fileManager: FileManager = .default) -> URL {
    fileManager.homeDirectoryForCurrentUser.appendingPathComponent(".wendy/cache/images")
}

private func listCachedImages(fileManager: FileManager = .default) throws -> [CachedImageEntry] {
    let root = cacheDirectory(fileManager: fileManager)
    guard fileManager.fileExists(atPath: root.path) else { return [] }

    let contents: [URL]
    do {
        contents = try fileManager.contentsOfDirectory(
            at: root,
            includingPropertiesForKeys: [.isDirectoryKey],
            options: [.skipsHiddenFiles]
        )
    } catch {
        throw ValidationError("Failed to read cache directory: \(error.localizedDescription)")
    }

    var entries: [CachedImageEntry] = []

    for url in contents {
        let values = try? url.resourceValues(forKeys: [.isDirectoryKey])
        guard values?.isDirectory == true else { continue }

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

        entries.append(
            CachedImageEntry(
                device: url.lastPathComponent,
                version: version,
                cachedAt: timestamp,
                imagePath: imageURL?.path,
                sizeBytes: size,
                status: status
            )
        )
    }

    return entries.sorted { $0.device < $1.device }
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
                .isRegularFileKey, .totalFileAllocatedSizeKey, .fileSizeKey,
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
