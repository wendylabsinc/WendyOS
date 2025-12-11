#if !os(Windows)
import AsyncHTTPClient
import Foundation
import NIOCore
import NIOFoundationCompat
import _NIOFileSystem

enum DownloadError: Error {
    case invalidResponse
}

public func downloadFile(
    from url: URL,
    to path: String,
    expectedSize: Int64? = nil,
    progressHandler: @escaping (Progress) -> Void
) async throws {
    var bytesDownloaded: Int64 = 0

    // Fire GET request
    let request = HTTPClientRequest(url: url.absoluteString)
    let response = try await HTTPClient.shared.execute(
        request,
        deadline: NIODeadline.now() + .seconds(60)
    )

    // Determine total size: prefer Content-Length, else expectedSize if provided
    let headerSize = response.headers.first(name: "Content-Length").flatMap(Int64.init)
    let totalSize = headerSize ?? expectedSize

    guard let totalSize, totalSize > 0 else {
        throw DownloadError.invalidResponse
    }

    let progress = Progress(totalUnitCount: totalSize)

    try await FileSystem.shared.withFileHandle(
        forWritingAt: FilePath(path),
        options: .newFile(replaceExisting: true)
    ) { handle in
        var writer = handle.bufferedWriter(startingAtAbsoluteOffset: 0)
        try await writer.flush()
        for try await chunk in response.body {
            let bytesWritten = try await writer.write(contentsOf: chunk)
            bytesDownloaded += Int64(bytesWritten)
            progress.completedUnitCount = bytesDownloaded
            progressHandler(progress)
        }

        try await writer.flush()
    }

    progress.completedUnitCount = totalSize
    progressHandler(progress)
}
#endif