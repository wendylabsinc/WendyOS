import AsyncHTTPClient
import DownloadSupport
import Foundation
import NIOCore
import _NIOFileSystem

#if os(macOS)
    import Darwin
#elseif os(Linux)
    // No explicit libc import needed when using musl
#endif

// MARK: - Protocols

/// Protocol defining image downloading functionality
public protocol ImageDownloading: Sendable {
    /// Downloads an image from a URL to a local temporary file and extracts the .img file if needed
    /// - Parameters:
    ///   - url: The URL to download from
    ///   - expectedSize: The expected size in bytes
    ///   - progressHandler: Closure to report download progress
    /// - Returns: The local file path where the image was saved
    func downloadImage(
        from url: URL,
        deviceName: String,
        expectedSize: Int,
        redownload: Bool,
        progressHandler: @escaping @Sendable (Progress) -> Void
    ) async throws -> (String, cached: Bool)
}

// MARK: - Implementations

/// Manages downloading device images from GCS
public actor ImageDownloader: ImageDownloading {
    private let fileManager: FileManager

    public init(fileManager: FileManager = .default) {
        self.fileManager = fileManager
    }

    private func extractImage(
        from path: String,
        to directory: String,
        progressHandler: @escaping (Progress) -> Void
    ) async throws -> String {
        // Prefer streaming a single .img for accurate progress. Fallback to unzip -o when needed.
        let unzipPath = try findExecutable(name: "unzip", standardPath: "/usr/bin/unzip")
        guard fileManager.fileExists(atPath: unzipPath) else {
            throw DownloadError.extractionFailed("Could not find 'unzip' utility on the system")
        }

        // Discover .img entry and its uncompressed size via `unzip -l`
        let listProc = Process()
        listProc.executableURL = URL(fileURLWithPath: unzipPath)
        listProc.arguments = ["-l", path]
        let listOut = Pipe()
        listProc.standardOutput = listOut
        listProc.standardError = Pipe()
        try listProc.run()
        listProc.waitUntilExit()

        func parseImgEntry(_ text: String) -> (entry: String, size: Int64)? {
            var candidate: (String, Int64)?
            text.split(separator: "\n").forEach { lineSub in
                let line = String(lineSub)
                guard line.lowercased().contains(".img") else { return }
                // Expect lines like: "  123456  mm-dd-yy  hh:mm   path/to/file.img"
                let parts = line.split(separator: " ", omittingEmptySubsequences: true)
                guard parts.count >= 4 else { return }
                if let size = Int64(parts[0]),
                    let nameStart = line.range(of: " ", options: .backwards)?.upperBound
                {
                    let name = String(line[nameStart...]).trimmingCharacters(in: .whitespaces)
                    if name.lowercased().hasSuffix(".img") {
                        candidate = (name, size)
                    }
                }
            }
            return candidate
        }

        let listText =
            String(data: listOut.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        if let (entryName, totalBytes) = parseImgEntry(listText) {
            // Stream unzip of that entry to a file while reporting precise byte progress.
            let destURL = URL(fileURLWithPath: directory).appendingPathComponent(
                (entryName as NSString).lastPathComponent
            )
            // Ensure destination directory exists
            try fileManager.createDirectory(atPath: directory, withIntermediateDirectories: true)
            if fileManager.fileExists(atPath: destURL.path) {
                try? fileManager.removeItem(at: destURL)
            }
            // Create an empty destination file so FileHandle can open it
            let created = fileManager.createFile(
                atPath: destURL.path,
                contents: nil,
                attributes: nil
            )
            if !created {
                throw DownloadError.extractionFailed(
                    "Failed to create destination file at \(destURL.path)"
                )
            }

            // Progress init
            let p = Progress(totalUnitCount: totalBytes)
            p.completedUnitCount = 0
            progressHandler(p)

            // Run `unzip -p path entryName` and stream stdout to file
            let unzipProc = Process()
            unzipProc.executableURL = URL(fileURLWithPath: unzipPath)
            unzipProc.arguments = ["-p", path, entryName]
            let outPipe = Pipe()
            unzipProc.standardOutput = outPipe
            unzipProc.standardError = Pipe()
            try unzipProc.run()

            let destHandle = try FileHandle(forWritingTo: destURL)
            try? destHandle.truncate(atOffset: 0)
            defer { try? destHandle.close() }

            while true {
                let data = outPipe.fileHandleForReading.readData(ofLength: 1 << 16)  // 64 KiB
                if data.isEmpty {
                    break
                }
                try destHandle.write(contentsOf: data)
                p.completedUnitCount += Int64(data.count)
                progressHandler(p)
            }

            unzipProc.waitUntilExit()
            if unzipProc.terminationStatus != 0 {
                throw DownloadError.extractionFailed("unzip failed while streaming .img entry")
            }

            // Finalize
            p.completedUnitCount = totalBytes
            progressHandler(p)
            // Best-effort cleanup: remove the zip to save space
            try? fileManager.removeItem(at: URL(fileURLWithPath: path))
            return destURL.path
        }

        // Fallback: unzip whole archive (no granular progress available); caller may overlay estimator.
        let unzipProcess = Process()
        unzipProcess.executableURL = URL(fileURLWithPath: unzipPath)
        unzipProcess.arguments = ["-o", path, "-d", directory]
        unzipProcess.standardOutput = Pipe()
        let errorPipe = Pipe()
        unzipProcess.standardError = errorPipe
        try unzipProcess.run()
        unzipProcess.waitUntilExit()
        if unzipProcess.terminationStatus != 0 {
            let errorMessage =
                String(data: errorPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8)
                ?? "Unknown error"
            throw DownloadError.extractionFailed(
                "Failed to extract ZIP file: \(errorMessage.trimmingCharacters(in: .whitespacesAndNewlines))"
            )
        }

        let imgPath = try await validateImage(at: directory)
        // Best-effort cleanup: remove the zip to save space
        try? fileManager.removeItem(at: URL(fileURLWithPath: path))
        return imgPath
    }

    private func validateImage(at directory: String) async throws -> String {
        // Find the .img file in the extracted directory
        let enumerator = fileManager.enumerator(
            at: URL(fileURLWithPath: directory),
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )

        var imgFilePath: String?
        while let fileURL = enumerator?.nextObject() as? URL {
            if fileURL.pathExtension.lowercased() == "img" {
                imgFilePath = fileURL.path
                break
            }
        }

        guard let imgPath = imgFilePath else {
            throw DownloadError.extractionFailed("No .img file found in the downloaded archive")
        }

        return imgPath
    }

    public func downloadImage(
        from url: URL,
        deviceName: String,
        expectedSize: Int,
        redownload: Bool = false,
        progressHandler: @escaping @Sendable (Progress) -> Void
    ) async throws -> (String, cached: Bool) {
        let cacheDir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(
            ".wendy/cache/images"
        )
        let extractionDirectoryURL = cacheDir.appendingPathComponent(deviceName)
        let temporaryDirectory = fileManager.temporaryDirectory
        let tempFilename = UUID().uuidString
        let localZipURL = temporaryDirectory.appendingPathComponent("\(tempFilename).zip")

        func redownloadImage() async throws -> String {
            // Composite progress: 0..0.98 download, 0.98..1.0 extraction
            let downloadWeight: Double = 0.98
            let extractWeight: Double = 1.0 - downloadWeight

            @inline(__always)
            func reportFraction(_ f: Double) {
                let clamped = max(0.0, min(f, 1.0))
                let p = Progress(totalUnitCount: 10_000)
                p.completedUnitCount = Int64((clamped * 10_000.0).rounded())
                progressHandler(p)
            }

            // 1) Download with progress mapped to 0..0.98
            try await downloadFile(
                from: url,
                to: localZipURL.path,
                expectedSize: Int64(expectedSize)
            ) { p in
                let total = max(1, p.totalUnitCount)
                let frac = Double(p.completedUnitCount) / Double(total)
                reportFraction(downloadWeight * frac)
            }

            // Ensure we created the extraction directory. If we're re-downloading,
            // clear any previous extraction to avoid unzip interactive prompts.
            if fileManager.fileExists(atPath: extractionDirectoryURL.path) {
                // Best-effort cleanup; ignore errors so we can recreate below
                try? fileManager.removeItem(at: extractionDirectoryURL)
            }
            try fileManager.createDirectory(
                at: extractionDirectoryURL,
                withIntermediateDirectories: true,
                attributes: nil
            )

            // 2) Extract with mapping 0.98..1.0 and a gentle estimator while unzip runs
            // Start a lightweight estimator that advances towards 0.995 while we extract
            let estimatorQueue = DispatchQueue(label: "wendy.extract.estimator")
            let estimator = DispatchSource.makeTimerSource(queue: estimatorQueue)
            let estStart = Date()
            estimator.schedule(deadline: .now() + 1.0, repeating: 0.5)
            estimator.setEventHandler {
                let elapsed = Date().timeIntervalSince(estStart)
                // Ease-in progress: ~1% over ~20s, capped at 99.5%
                let est = min(0.995, 0.98 + min(0.02, 0.001 * elapsed))
                reportFraction(est)
            }
            estimator.resume()

            defer { estimator.cancel() }

            let resultPath = try await extractImage(
                from: localZipURL.path,
                to: extractionDirectoryURL.path
            ) { p in
                // Map extractor's 0..100 to 0.98..1.0
                let total = max(1, p.totalUnitCount)
                let frac = Double(p.completedUnitCount) / Double(total)
                reportFraction(downloadWeight + extractWeight * frac)
            }

            // Force 100% on completion
            reportFraction(1.0)
            return resultPath
        }

        let isValidCache =
            try
            (!fileManager.fileExists(atPath: extractionDirectoryURL.path)
            || FileManager.default.contentsOfDirectory(atPath: extractionDirectoryURL.path).isEmpty)

        if redownload || isValidCache {
            return (try await redownloadImage(), cached: false)
        } else {
            print("Using cached image for \(deviceName)")

            do {
                return (try await validateImage(at: extractionDirectoryURL.path), cached: true)
            } catch {
                print("Invalid image found in cache, redownloading...")

                return (try await redownloadImage(), cached: false)
            }
        }
    }

    // MARK: - New phased APIs

    /// Stores version metadata for a cached image
    private func storeVersionMetadata(deviceName: String, version: String) throws {
        let cacheDir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(
            ".wendy/cache/images"
        )
        let metadataURL = cacheDir.appendingPathComponent(deviceName).appendingPathComponent("version.json")

        let metadata = ["version": version, "timestamp": ISO8601DateFormatter().string(from: Date())]
        let data = try JSONSerialization.data(withJSONObject: metadata, options: .prettyPrinted)
        try data.write(to: metadataURL)
    }

    /// Reads version metadata for a cached image
    private nonisolated func readVersionMetadata(deviceName: String) -> String? {
        let cacheDir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(
            ".wendy/cache/images"
        )
        let metadataURL = cacheDir.appendingPathComponent(deviceName).appendingPathComponent("version.json")

        guard let data = try? Data(contentsOf: metadataURL),
              let metadata = try? JSONSerialization.jsonObject(with: data) as? [String: String] else {
            return nil
        }
        return metadata["version"]
    }

    /// Returns a valid cached .img path if available, else nil.
    public func cachedImageIfValid(deviceName: String) async -> String? {
        let cacheDir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(
            ".wendy/cache/images"
        )
        let extractionDirectoryURL = cacheDir.appendingPathComponent(deviceName)
        do {
            return try await validateImage(at: extractionDirectoryURL.path)
        } catch {
            return nil
        }
    }

    /// Checks if cached image version matches the latest version
    public nonisolated func isCachedImageLatest(deviceName: String, latestVersion: String) -> Bool {
        guard let cachedVersion = readVersionMetadata(deviceName: deviceName) else {
            return false
        }
        return cachedVersion == latestVersion
    }

    /// Download the archive only, reporting progress. Returns the zip path and the extraction directory path.
    public func downloadArchiveOnly(
        from url: URL,
        deviceName: String,
        expectedSize: Int,
        redownload: Bool,
        version: String? = nil,
        progressHandler: @escaping @Sendable (Progress) -> Void
    ) async throws -> (zipPath: String, extractionDir: String) {
        let cacheDir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(
            ".wendy/cache/images"
        )
        let extractionDirectoryURL = cacheDir.appendingPathComponent(deviceName)
        let temporaryDirectory = fileManager.temporaryDirectory
        let tempFilename = UUID().uuidString
        let localZipURL = temporaryDirectory.appendingPathComponent("\(tempFilename).zip")
        
        try await downloadFile(
            from: url,
            to: localZipURL.path,
            expectedSize: Int64(expectedSize),
            progressHandler: progressHandler
        )

        return (zipPath: localZipURL.path, extractionDir: extractionDirectoryURL.path)
    }

    /// Extract a previously downloaded archive into the cache directory for the device.
    public func extractArchiveOnly(
        deviceName: String,
        zipPath: String,
        version: String? = nil,
        progressHandler: @escaping (Progress) -> Void
    ) async throws -> String {
        let cacheDir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(
            ".wendy/cache/images"
        )
        let extractionDirectoryURL = cacheDir.appendingPathComponent(deviceName)

        // Prepare extraction dir: clear if exists, then recreate
        if fileManager.fileExists(atPath: extractionDirectoryURL.path) {
            try? fileManager.removeItem(at: extractionDirectoryURL)
        }
        try fileManager.createDirectory(
            at: extractionDirectoryURL,
            withIntermediateDirectories: true,
            attributes: nil
        )

        // Start a gentle estimator to avoid a "stuck at 0%" feel while unzip runs.
        let estimatorQueue = DispatchQueue(label: "wendy.extract.progress")
        let estimator = DispatchSource.makeTimerSource(queue: estimatorQueue)
        let start = Date()
        estimator.schedule(deadline: .now() + 1.0, repeating: 0.5)
        estimator.setEventHandler {
            let elapsed = Date().timeIntervalSince(start)
            // Progress rises slowly up to 95% while unzip works.
            // ~1% per second with a small head-start; capped at 95%.
            let estFraction = min(0.95, 0.02 + 0.01 * elapsed)
            let p = Progress(totalUnitCount: 100)
            p.completedUnitCount = Int64((estFraction * 100.0).rounded())
            progressHandler(p)
        }
        estimator.resume()

        defer { estimator.cancel() }

        let resultPath = try await extractImage(
            from: zipPath,
            to: extractionDirectoryURL.path,
            progressHandler: progressHandler
        )

        // Force 100%
        let p = Progress(totalUnitCount: 100)
        p.completedUnitCount = 100
        progressHandler(p)

        // Store version metadata after successful extraction
        if let version = version {
            try? storeVersionMetadata(deviceName: deviceName, version: version)
        }

        return resultPath
    }
}

// MARK: - Factory

/// Factory for creating ImageDownloader instances
public enum ImageDownloaderFactory {
    /// Creates and returns a default ImageDownloader instance
    public static func createImageDownloader() -> ImageDownloading {
        return ImageDownloader()
    }
}

// MARK: - Errors

/// Errors related to download operations
public enum DownloadError: Error, LocalizedError {
    case fileCreationFailed
    case invalidResponse
    case downloadFailed(Error)
    case extractionFailed(String)

    public var errorDescription: String? {
        switch self {
        case .fileCreationFailed:
            return "Failed to create local file for download"
        case .invalidResponse:
            return "Invalid response from server"
        case .downloadFailed(let error):
            return "Download failed: \(error.localizedDescription)"
        case .extractionFailed(let reason):
            return "Failed to extract image file: \(reason)"
        }
    }
}
