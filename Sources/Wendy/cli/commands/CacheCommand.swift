import ArgumentParser
import CLIOutput
import Foundation
import Logging

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

            cliOutput.table(
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
                let cacheHint = primaryCacheDirectory().path
                print(
                    "\nSome cache entries look incomplete. Remove the corresponding folders under "
                        + "\(cacheHint) if you need to reclaim space."
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
                    cliOutput.info("No cached WendyOS images to clear.")
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

                let selectedIndex = try await cliOutput.selectFromTable(
                    title: nil,
                    headers: [
                        "Device",
                        "Version",
                        "Status",
                        "Size",
                    ],
                    rows: rows,
                    pageSize: 20
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
                let confirmed = try await cliOutput.yesOrNoPrompt(
                    question: "Remove \(description)? (~\(sizeText))",
                    defaultAnswer: false
                )
                guard confirmed else {
                    cliOutput.info("Aborted.")
                    return
                }
            }

            var failures: [(String, String)] = []
            let roots = cacheDirectories(fileManager: fileManager)

            for entry in targets {
                for root in roots {
                    let path = root.appendingPathComponent(entry.device)
                    do {
                        if fileManager.fileExists(atPath: path.path) {
                            try fileManager.removeItem(at: path)
                        }
                    } catch {
                        failures.append((entry.device, error.localizedDescription))
                    }
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
                    cliOutput.success("Cleared all cached images (\(sizeText)).")
                } else {
                    let names = targets.map(\.device).joined(separator: ", ")
                    cliOutput.success("Cleared cache for \(names) (\(sizeText)).")
                }
            } else {
                for failure in failures {
                    cliOutput.error("Failed to clear cache for \(failure.0): \(failure.1)")
                }
            }
        }
    }
}
