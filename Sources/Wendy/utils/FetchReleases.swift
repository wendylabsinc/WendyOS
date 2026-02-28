import AsyncHTTPClient
import DownloadSupport
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import Subprocess

#if os(macOS)
    import Darwin
#elseif canImport(Musl)
    import Musl
#elseif canImport(Glibc)
    import Glibc
#endif

/// Protocol for executing HTTP requests, enabling dependency injection for testing
protocol HTTPExecutor {
    func execute(
        _ request: HTTPClientRequest,
        deadline: NIODeadline
    ) async throws -> HTTPClientResponse
}

/// Default implementation using HTTPClient.shared
struct DefaultHTTPExecutor: HTTPExecutor {
    private let client: HTTPClient

    init(client: HTTPClient = .shared) {
        self.client = client
    }

    func execute(
        _ request: HTTPClientRequest,
        deadline: NIODeadline
    ) async throws -> HTTPClientResponse {
        try await client.execute(request, deadline: deadline)
    }
}

struct Release: Decodable {
    struct Asset: Decodable {
        let browser_download_url: String
        let name: String
        let content_type: String
    }
    let prerelease: Bool
    let assets: [Asset]
    let name: String
}

enum ReleasesError: Error {
    case invalidResponse
    case noReleases
    case noAsset
    case unsupportedPlatform(String)
    case invalidDownloadURL(String)
    case fileTooLarge(actual: Int64, maximum: Int64)
    case rateLimitExceeded(resetTime: Date)
}

/// Supported platforms and architectures
enum Platform: String {
    case linuxAarch64 = "linux-static-musl-aarch64"
    case linuxX86_64 = "linux-static-musl-x86_64"
    case macosArm64 = "macos-arm64"

    /// Maps a device-reported CPU architecture string to a Linux platform
    static func linuxPlatform(forArchitecture arch: String) throws -> Platform {
        switch arch.lowercased() {
        case "aarch64", "arm64":
            return .linuxAarch64
        case "x86_64", "amd64":
            return .linuxX86_64
        default:
            throw ReleasesError.unsupportedPlatform("Unsupported device architecture: \(arch)")
        }
    }

    /// Detects the current platform
    static func current() throws -> Platform {
        #if os(macOS)
            #if arch(arm64)
                return .macosArm64
            #else
                throw ReleasesError.unsupportedPlatform("macOS x86_64 is not supported")
            #endif
        #elseif os(Linux)
            #if arch(arm64)
                return .linuxAarch64
            #elseif arch(x86_64)
                return .linuxX86_64
            #else
                throw ReleasesError.unsupportedPlatform(
                    "Linux platform not supported: unknown architecture"
                )
            #endif
        #else
            throw ReleasesError.unsupportedPlatform("Platform not supported")
        #endif
    }
}

#if !os(Windows)
    func downloadLatestRelease(
        httpClient: HTTPExecutor = DefaultHTTPExecutor(),
        platform: Platform = .linuxAarch64,
        includePrerelease: Bool = false
    ) async throws -> URL {
        // Fetch all releases
        let releases = try await fetchReleases(httpClient: httpClient)

        // Filter releases based on prerelease preference
        let filteredReleases: [Release]
        if includePrerelease {
            // Include all releases (both stable and pre-releases)
            filteredReleases = releases
        } else {
            // Only include stable releases (non-prerelease)
            filteredReleases = releases.filter { !$0.prerelease }
        }

        guard let latestRelease = filteredReleases.first else {
            throw ReleasesError.noReleases
        }

        // Build the expected asset name pattern
        // Format: wendy-agent-{platform}-{version}.tar.gz
        // Example: wendy-agent-linux-static-musl-aarch64-v0.2.0.tar.gz
        let assetPrefix = "wendy-agent-\(platform.rawValue)-"
        let assetSuffix = ".tar.gz"

        guard
            let asset = latestRelease.assets.first(where: { asset in
                asset.name.hasPrefix(assetPrefix) && asset.name.hasSuffix(assetSuffix)
            })
        else {
            throw ReleasesError.noAsset
        }

        let downloadedFileURL = try await downloadAsset(asset, httpClient: httpClient)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
            UUID().uuidString
        )

        let fileURL = try await extract(at: downloadedFileURL, to: directory) { file in
            file.lastPathComponent == "wendy-agent"
        }
        try? FileManager.default.removeItem(at: downloadedFileURL)
        return fileURL
    }
#endif

func fetchReleases(httpClient: HTTPExecutor = DefaultHTTPExecutor()) async throws -> [Release] {
    let githubReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-agent/releases"

    // Fetch releases JSON
    let logger = Logger(label: "sh.wendy.utils.fetchReleases")
    logger.debug("Fetching all releases...")

    var request = HTTPClientRequest(url: githubReleasesURL)
    request.headers.add(name: "Accept", value: "application/vnd.github+json")
    request.headers.add(name: "X-GitHub-Api-Version", value: "2022-11-28")
    request.headers.add(name: "User-Agent", value: "wendy-agent")
    let response = try await httpClient.execute(
        request,
        deadline: NIODeadline.now() + .seconds(60)
    )

    // Check for successful response
    guard response.status == .ok else {
        // Check for rate limiting (HTTP 403)
        if response.status.code == 403 {
            // Check if this is a rate limit error
            if let rateLimitRemaining = response.headers.first(name: "X-RateLimit-Remaining"),
                rateLimitRemaining == "0",
                let rateLimitReset = response.headers.first(name: "X-RateLimit-Reset"),
                let resetTimestamp = TimeInterval(rateLimitReset)
            {
                let resetDate = Date(timeIntervalSince1970: resetTimestamp)
                let timeUntilReset = resetDate.timeIntervalSinceNow

                logger.error(
                    "GitHub API rate limit exceeded",
                    metadata: [
                        "reset_time": "\(resetDate)",
                        "minutes_until_reset": "\(Int(timeUntilReset / 60))",
                    ]
                )
                throw ReleasesError.rateLimitExceeded(resetTime: resetDate)
            }
        }

        logger.error("Failed to fetch releases: HTTP error - status \(response.status)")
        throw ReleasesError.invalidResponse
    }

    // Log rate limit info for monitoring
    if let rateLimitRemaining = response.headers.first(name: "X-RateLimit-Remaining"),
        let rateLimitLimit = response.headers.first(name: "X-RateLimit-Limit")
    {
        logger.debug(
            "GitHub API rate limit status",
            metadata: [
                "remaining": "\(rateLimitRemaining)",
                "limit": "\(rateLimitLimit)",
            ]
        )
    }

    // Collect response body
    let body = try await response.body.collect(upTo: 10 * 1024 * 1024)  // 10MB limit
    let data = Data(buffer: body)

    return try JSONDecoder().decode([Release].self, from: data)
}

#if !os(Windows)
    func downloadAsset(
        _ asset: Release.Asset,
        httpClient: HTTPExecutor = DefaultHTTPExecutor()
    ) async throws -> URL {
        let fileManager = FileManager.default
        let tempDir = fileManager.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try fileManager.createDirectory(at: tempDir, withIntermediateDirectories: true)

        guard let downloadURL = URL(string: asset.browser_download_url) else {
            throw ReleasesError.invalidDownloadURL(asset.browser_download_url)
        }

        // Check file size before downloading (500MB limit)
        let maxFileSize: Int64 = 500 * 1024 * 1024  // 500MB in bytes
        let logger = Logger(label: "sh.wendy.utils.fetchReleases")

        // Make HEAD request to get Content-Length
        var headRequest = HTTPClientRequest(url: downloadURL.absoluteString)
        headRequest.method = .HEAD
        let headResponse = try await httpClient.execute(
            headRequest,
            deadline: NIODeadline.now() + .seconds(30)
        )

        if let contentLength = headResponse.headers.first(name: "Content-Length"),
            let fileSize = Int64(contentLength)
        {
            if fileSize > maxFileSize {
                logger.error(
                    "Asset file too large",
                    metadata: [
                        "asset": "\(asset.name)",
                        "size_mb": "\(fileSize / 1024 / 1024)",
                        "max_mb": "\(maxFileSize / 1024 / 1024)",
                    ]
                )
                throw ReleasesError.fileTooLarge(actual: fileSize, maximum: maxFileSize)
            }
            logger.debug(
                "Downloading asset",
                metadata: [
                    "asset": "\(asset.name)",
                    "size_mb": "\(fileSize / 1024 / 1024)",
                ]
            )
        } else {
            logger.warning(
                "Could not determine file size before download",
                metadata: [
                    "asset": "\(asset.name)"
                ]
            )
        }

        let downloadedFileURL = tempDir.appendingPathComponent(asset.name)
        try await downloadFile(from: downloadURL, to: downloadedFileURL.path) { _ in }
        logger.debug("Downloaded asset", metadata: ["path": "\(downloadedFileURL.path)"])
        return downloadedFileURL
    }
#endif

enum ExtractError: Error {
    case failedToExtract
    case executableNotFound
    case maliciousArchive(String)
}

func extract(
    at url: URL,
    to tempDir: URL,
    findExecutable: (URL) -> Bool
) async throws -> URL {
    let logger = Logger(label: "sh.wendy.utils.fetchReleases")

    // Determine if it's a tar.gz or a binary
    let isTarGz = url.pathExtension == "gz" || url.lastPathComponent.contains("tar.gz")
    guard isTarGz else {
        logger.debug(
            "File is not a tar.gz file, returning as-is",
            metadata: ["path": "\(url.path)"]
        )
        return url
    }

    logger.info("Extracting tar.gz archive", metadata: ["path": "\(url.path)"])
    let extractDir = tempDir.appendingPathComponent("extract")
    try FileManager.default.createDirectory(at: extractDir, withIntermediateDirectories: true)

    // Validate tar contents before extraction to prevent path traversal attacks
    logger.debug("Validating tar archive contents")
    let listResult = try await Subprocess.run(
        Subprocess.Executable.name("tar"),
        arguments: Subprocess.Arguments(["-tzf", url.path]),
        output: .string(limit: .max),
        error: .string(limit: .max)
    )

    guard listResult.terminationStatus.isSuccess else {
        let errorMessage = listResult.standardError ?? "unknown error"
        logger.error(
            "Failed to list tar archive contents",
            metadata: [
                "termination_status": "\(listResult.terminationStatus)",
                "archive": "\(url.path)",
                "error": "\(errorMessage)",
            ]
        )
        throw ExtractError.failedToExtract
    }

    let output = listResult.standardOutput ?? ""
    let files = output.components(separatedBy: .newlines).filter { !$0.isEmpty }

    // Check each file path for security issues
    for filePath in files {
        // Check for absolute paths
        if filePath.hasPrefix("/") {
            logger.error(
                "Archive contains absolute path",
                metadata: ["path": "\(filePath)"]
            )
            throw ExtractError.maliciousArchive("Archive contains absolute path: \(filePath)")
        }

        // Check for path traversal sequences (.. anywhere in the path)
        if filePath.contains("..") {
            logger.error(
                "Archive contains path traversal sequence",
                metadata: ["path": "\(filePath)"]
            )
            throw ExtractError.maliciousArchive("Archive contains path traversal: \(filePath)")
        }
    }

    // Extract the archive
    let extractResult = try await Subprocess.run(
        Subprocess.Executable.name("tar"),
        arguments: Subprocess.Arguments(["-xzf", url.path, "-C", extractDir.path]),
        output: .string(limit: .max),
        error: .string(limit: .max)
    )

    guard extractResult.terminationStatus.isSuccess else {
        let errorMessage = extractResult.standardError ?? "unknown error"
        logger.error(
            "Failed to extract tar.gz archive",
            metadata: [
                "termination_status": "\(extractResult.terminationStatus)",
                "archive": "\(url.path)",
                "error": "\(errorMessage)",
            ]
        )
        throw ExtractError.failedToExtract
    }

    // Find the binary in the extracted directory with depth limit
    // Limit to 5 levels deep to prevent malicious archives from causing excessive traversal
    let maxDepth = 5

    func findExecutableRecursive(in directory: URL, depth: Int) throws -> URL? {
        guard depth < maxDepth else {
            logger.warning(
                "Reached maximum directory depth",
                metadata: ["max_depth": "\(maxDepth)", "path": "\(directory.path)"]
            )
            return nil
        }

        let contents = try FileManager.default.contentsOfDirectory(
            at: directory,
            includingPropertiesForKeys: [.isDirectoryKey],
            options: [.skipsHiddenFiles]
        )

        // First check files in current directory
        for file in contents {
            let resourceValues = try file.resourceValues(forKeys: [.isDirectoryKey])
            if resourceValues.isDirectory != true && findExecutable(file) {
                return file
            }
        }

        // Then recurse into subdirectories
        for file in contents {
            let resourceValues = try file.resourceValues(forKeys: [.isDirectoryKey])
            if resourceValues.isDirectory == true {
                if let found = try findExecutableRecursive(in: file, depth: depth + 1) {
                    return found
                }
            }
        }

        return nil
    }

    if let executableURL = try findExecutableRecursive(in: extractDir, depth: 0) {
        return executableURL
    }

    throw ExtractError.executableNotFound
}
