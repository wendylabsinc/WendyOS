import AsyncHTTPClient
import DownloadSupport
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import Subprocess

#if os(macOS)
    import Darwin
#elseif os(Linux)
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
}

/// Result of downloading a release containing the binary and temp directory for cleanup
struct DownloadedRelease {
    let binaryURL: URL
    let tempDirectory: URL
}

/// Supported platforms and architectures
enum Platform: String {
    case linuxAarch64 = "linux-static-musl-aarch64"
    case linuxX86_64 = "linux-static-musl-x86_64"
    case macosArm64 = "macos-arm64"

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

func downloadLatestRelease(
    httpClient: HTTPExecutor = DefaultHTTPExecutor(),
    platform: Platform? = nil,
    includePrerelease: Bool = false
) async throws -> DownloadedRelease {
    // Detect platform if not specified
    let targetPlatform = try platform ?? Platform.current()

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
    let assetPrefix = "wendy-agent-\(targetPlatform.rawValue)-"
    let assetSuffix = ".tar.gz"

    guard
        let asset = latestRelease.assets.first(where: { asset in
            asset.name.hasPrefix(assetPrefix) && asset.name.hasSuffix(assetSuffix)
        })
    else {
        throw ReleasesError.noAsset
    }

    let downloadedFileURL = try await downloadAsset(asset)
    // Create root temp directory for this download
    let tempDirectory = FileManager.default.temporaryDirectory.appendingPathComponent(
        UUID().uuidString
    )

    // Track if we should clean up on failure
    var shouldCleanupOnFailure = true
    defer {
        if shouldCleanupOnFailure {
            // Clean up temp directory if we failed before completing
            do {
                try FileManager.default.removeItem(at: tempDirectory)
            } catch {
                let logger = Logger(label: "sh.wendy.utils.fetchReleases")
                logger.warning(
                    "Failed to clean up temp directory on failure",
                    metadata: [
                        "path": "\(tempDirectory.path)",
                        "error": "\(error)",
                    ]
                )
            }
        }
    }

    let binaryURL = try await extract(at: downloadedFileURL, to: tempDirectory) { file in
        file.lastPathComponent == "wendy-agent"
    }

    // Clean up downloaded archive
    do {
        try FileManager.default.removeItem(at: downloadedFileURL)
    } catch {
        let logger = Logger(label: "sh.wendy.utils.fetchReleases")
        logger.warning(
            "Failed to remove downloaded archive",
            metadata: [
                "path": "\(downloadedFileURL.path)",
                "error": "\(error)",
            ]
        )
    }

    // Success - don't clean up, let caller handle it
    shouldCleanupOnFailure = false
    return DownloadedRelease(binaryURL: binaryURL, tempDirectory: tempDirectory)
}

func fetchReleases(httpClient: HTTPExecutor = DefaultHTTPExecutor()) async throws -> [Release] {
    let githubReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-agent/releases"

    // Fetch releases JSON
    let logger = Logger(label: "sh.wendy.utils.fetchReleases")
    logger.info("Fetching all releases...")

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
        logger.error("Failed to fetch releases: HTTP error - status \(response.status)")
        throw ReleasesError.invalidResponse
    }

    // Collect response body
    let body = try await response.body.collect(upTo: 10 * 1024 * 1024)  // 10MB limit
    let data = Data(buffer: body)

    return try JSONDecoder().decode([Release].self, from: data)
}

func downloadAsset(_ asset: Release.Asset) async throws -> URL {
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
    let headResponse = try await HTTPClient.shared.execute(
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
        logger.info(
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
    logger.info("Downloaded asset", metadata: ["path": "\(downloadedFileURL.path)"])
    return downloadedFileURL
}

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
    let listProcess = Process()
    listProcess.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    listProcess.arguments = ["tar", "-tzf", url.path]

    let pipe = Pipe()
    listProcess.standardOutput = pipe
    let errorPipe = Pipe()
    listProcess.standardError = errorPipe

    try listProcess.run()
    listProcess.waitUntilExit()

    guard listProcess.terminationStatus == 0 else {
        let errorData = errorPipe.fileHandleForReading.readDataToEndOfFile()
        let errorMessage = String(data: errorData, encoding: .utf8) ?? "unknown error"
        logger.error(
            "Failed to list tar archive contents",
            metadata: [
                "exit_code": "\(listProcess.terminationStatus)",
                "archive": "\(url.path)",
                "error": "\(errorMessage)",
            ]
        )
        throw ExtractError.failedToExtract
    }

    let outputData = pipe.fileHandleForReading.readDataToEndOfFile()
    let output = String(data: outputData, encoding: .utf8) ?? ""
    let files = output.components(separatedBy: .newlines).filter { !$0.isEmpty }

    // Check each file path for security issues
    for filePath in files {
        // Check for absolute paths
        if filePath.hasPrefix("/") {
            logger.error(
                "Archive contains absolute path",
                metadata: [
                    "archive": "\(url.path)",
                    "malicious_path": "\(filePath)",
                ]
            )
            throw ExtractError.maliciousArchive("Archive contains absolute path: \(filePath)")
        }

        // Check for path traversal attempts
        if filePath.contains("../") || filePath.contains("/..") {
            logger.error(
                "Archive contains path traversal attempt",
                metadata: [
                    "archive": "\(url.path)",
                    "malicious_path": "\(filePath)",
                ]
            )
            throw ExtractError.maliciousArchive(
                "Archive contains path traversal: \(filePath)"
            )
        }

        // Check for paths that would escape extraction directory when normalized
        let normalizedPath = (filePath as NSString).standardizingPath
        if normalizedPath.hasPrefix("/") || normalizedPath.contains("..") {
            logger.error(
                "Archive contains normalized path that escapes extraction directory",
                metadata: [
                    "archive": "\(url.path)",
                    "original_path": "\(filePath)",
                    "normalized_path": "\(normalizedPath)",
                ]
            )
            throw ExtractError.maliciousArchive(
                "Archive contains escaping path: \(filePath)"
            )
        }
    }

    logger.debug(
        "Tar archive validation passed",
        metadata: [
            "file_count": "\(files.count)"
        ]
    )

    // Ensure cleanup on failure
    var shouldCleanup = true
    defer {
        if shouldCleanup {
            do {
                try FileManager.default.removeItem(at: extractDir)
            } catch {
                let logger = Logger(label: "sh.wendy.utils.fetchReleases")
                logger.warning(
                    "Failed to clean up extraction directory on failure",
                    metadata: [
                        "path": "\(extractDir.path)",
                        "error": "\(error)",
                    ]
                )
            }
        }
    }

    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    process.arguments = ["tar", "-xzf", url.path, "-C", extractDir.path]

    // Capture stderr for better debugging
    let extractErrorPipe = Pipe()
    process.standardError = extractErrorPipe

    try process.run()
    process.waitUntilExit()

    guard process.terminationStatus == 0 else {
        let errorData = extractErrorPipe.fileHandleForReading.readDataToEndOfFile()
        let errorMessage = String(data: errorData, encoding: .utf8) ?? "unknown error"
        logger.error(
            "Failed to extract tar.gz archive",
            metadata: [
                "exit_code": "\(process.terminationStatus)",
                "archive": "\(url.path)",
                "error": "\(errorMessage)",
            ]
        )
        throw ExtractError.failedToExtract
    }

    // Find the binary in the extracted directory with depth limit
    // Limit to 5 levels deep to prevent malicious archives from causing excessive traversal
    let maxDepth = 5
    let enumerator = FileManager.default.enumerator(
        at: extractDir,
        includingPropertiesForKeys: [.isDirectoryKey]
    )

    while let file = enumerator?.nextObject() as? URL {
        // Calculate depth relative to extraction directory
        let relativePath = file.path.replacingOccurrences(
            of: extractDir.path + "/",
            with: ""
        )
        let depth = relativePath.components(separatedBy: "/").count

        // Skip if exceeding depth limit
        if depth > maxDepth {
            logger.debug(
                "Skipping file beyond depth limit",
                metadata: [
                    "file": "\(file.path)",
                    "depth": "\(depth)",
                    "max_depth": "\(maxDepth)",
                ]
            )
            enumerator?.skipDescendants()
            continue
        }

        if findExecutable(file) {
            // Found the executable, don't clean up
            shouldCleanup = false
            return file
        }
    }

    logger.error(
        "Executable not found in archive",
        metadata: [
            "extract_dir": "\(extractDir.path)",
            "max_depth": "\(maxDepth)",
        ]
    )
    throw ExtractError.executableNotFound
}
