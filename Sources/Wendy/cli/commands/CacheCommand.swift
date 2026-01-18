import ArgumentParser
import Foundation
import Logging
import Noora

struct CacheCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "cache",
        abstract: "Inspect and clear caches",
        subcommands: [
            ListCommand.self,
            ClearCommand.self,
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

        func run() async throws {
            let cachedImages = try listCachedImages()

            if JSONMode.isEnabled {
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

    struct ClearCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "clear",
            abstract: "Remove cached WendyOS images by device or all."
        )

        @Argument(
            help: "Device name to clear. Omit to select from existing cached images."
        )
        var device: String?

        @Flag(name: .long, help: "Remove all cached images.")
        var all: Bool = false

        @Flag(name: .long, help: "Skip confirmation prompts")
        var force: Bool = false

        func run() async throws {
            // In JSON mode, require either --all or a device argument
            if JSONMode.isEnabled && !all && device == nil {
                jsonModeRequiresArgument(
                    argument: "device",
                    description: "Provide a device name or use --all to clear all cached images"
                )
            }

            guard all || device != nil else {
                throw ValidationError("Specify a device name or --all.")
            }

            let noora = Noora()
            let fileManager = FileManager.default
            let cachedImages = try listCachedImages(fileManager: fileManager)

            guard !cachedImages.isEmpty else {
                if JSONMode.isEnabled {
                    struct ClearResult: Codable {
                        let success: Bool
                        let message: String
                    }
                    let result = ClearResult(
                        success: true,
                        message: "No cached WendyOS images to clear."
                    )
                    let data = try JSONEncoder().encode(result)
                    print(String(data: data, encoding: .utf8)!)
                } else {
                    noora.info("No cached WendyOS images to clear.")
                }
                return
            }

            let targets: [CachedImageEntry]
            if all {
                targets = cachedImages
            } else if let device {
                guard let entry = cachedImages.first(where: { $0.device == device }) else {
                    if JSONMode.isEnabled {
                        JSONErrorResponse(
                            error: "device_not_found",
                            reason: "No cached image found for device '\(device)'"
                        ).print()
                        return
                    }
                    throw ValidationError("No cached image found for device '\(device)'.")
                }
                targets = [entry]
            } else {
                // This branch is only reachable in non-JSON mode
                let rows = cachedImages.map { entry in
                    [
                        entry.device,
                        entry.version ?? "—",
                        entry.status.displayValue,
                        ByteCountFormatter.string(
                            fromByteCount: entry.sizeBytes,
                            countStyle: .file
                        ),
                    ]
                }

                let selectedIndex = try await noora.selectableTable(
                    headers: [
                        "Device",
                        "Version",
                        "Status",
                        "Size",
                    ],
                    rows: rows,
                    pageSize: rows.count
                )

                targets = [cachedImages[selectedIndex]]
            }

            let totalBytes = targets.reduce(0) { $0 + $1.sizeBytes }
            // Skip confirmation in JSON mode (use --force for non-interactive)
            if !force && !JSONMode.isEnabled {
                let description: String
                if all {
                    description = "all cached images"
                } else if targets.count == 1 {
                    description = "the cache for \(targets[0].device)"
                } else {
                    description = "the selected caches"
                }
                let sizeText = ByteCountFormatter.string(
                    fromByteCount: totalBytes,
                    countStyle: .file
                )
                let confirmed = noora.yesOrNoChoicePrompt(
                    question: "Remove \(description)? (~\(sizeText))",
                    defaultAnswer: false
                )
                guard confirmed else {
                    noora.info("Aborted.")
                    return
                }
            }

            let root = cacheDirectory(fileManager: fileManager)
            var failures: [(String, String)] = []

            for entry in targets {
                let path = root.appendingPathComponent(entry.device)
                do {
                    if fileManager.fileExists(atPath: path.path) {
                        try fileManager.removeItem(at: path)
                    }
                } catch {
                    failures.append((entry.device, error.localizedDescription))
                }
            }

            if JSONMode.isEnabled {
                struct ClearResult: Codable {
                    let success: Bool
                    let cleared: [String]
                    let freedBytes: Int64
                    let failures: [FailureInfo]?

                    struct FailureInfo: Codable {
                        let device: String
                        let error: String
                    }
                }

                let result = ClearResult(
                    success: failures.isEmpty,
                    cleared: targets.filter { t in !failures.contains { $0.0 == t.device } }.map(
                        \.device
                    ),
                    freedBytes: totalBytes,
                    failures: failures.isEmpty
                        ? nil : failures.map { ClearResult.FailureInfo(device: $0.0, error: $0.1) }
                )
                let data = try JSONEncoder().encode(result)
                print(String(data: data, encoding: .utf8)!)
            } else if failures.isEmpty {
                let sizeText = ByteCountFormatter.string(
                    fromByteCount: totalBytes,
                    countStyle: .file
                )
                if all {
                    noora.success("Cleared all cached images (\(sizeText)).")
                } else {
                    let names = targets.map(\.device).joined(separator: ", ")
                    noora.success("Cleared cache for \(names) (\(sizeText)).")
                }
            } else {
                for failure in failures {
                    noora.error("Failed to clear cache for \(failure.0): \(failure.1)")
                }
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
