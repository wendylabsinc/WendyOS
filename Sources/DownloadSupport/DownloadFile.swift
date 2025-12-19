
import Foundation
import NIOCore
import NIOFoundationCompat
import Synchronization
#if os(Windows)
import FoundationNetworking
#else
import AsyncHTTPClient
import _NIOFileSystem
#endif

enum DownloadError: Error {
    case invalidResponse
}

public func downloadFile(
    from url: URL,
    to path: String,
    expectedSize: Int64? = nil,
    progressHandler: @escaping @Sendable (Progress) -> Void
) async throws {
    #if !os(Windows)
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
    #else
    // Stream the response to disk to avoid large allocations on Windows
    final class WindowsDownloadDelegate: NSObject, URLSessionDataDelegate, Sendable {
        let expectedSize: Int64?
        let fileHandle: FileHandle
        let progress: Progress
        let progressHandler: @Sendable (Progress) -> Void
        let bytesDownloaded = Mutex<Int64>(0)
        let error = Mutex<Error?>(nil)
        let continuation = Mutex<CheckedContinuation<Void, Error>?>(nil)

        init(
            expectedSize: Int64?,
            fileHandle: FileHandle,
            progressHandler: @escaping @Sendable (Progress) -> Void
        ) {
            self.expectedSize = expectedSize
            self.fileHandle = fileHandle
            self.progressHandler = progressHandler
            self.progress = Progress(totalUnitCount: expectedSize ?? 0)
        }

        func urlSession(
            _ session: URLSession,
            dataTask: URLSessionDataTask,
            didReceive response: URLResponse,
            completionHandler: @escaping (URLSession.ResponseDisposition) -> Void
        ) {
            guard
                let http = response as? HTTPURLResponse,
                (200..<300).contains(http.statusCode)
            else {
                error.withLock { $0 = DownloadError.invalidResponse }
                completionHandler(.cancel)
                return
            }

            let headerSize = http.expectedContentLength > 0 ? http.expectedContentLength : expectedSize ?? -1
            guard headerSize > 0 else {
                error.withLock { $0 = DownloadError.invalidResponse }
                completionHandler(.cancel)
                return
            }

            progress.totalUnitCount = headerSize
            completionHandler(.allow)
        }

        func urlSession(
            _ session: URLSession,
            dataTask: URLSessionDataTask,
            didReceive data: Data
        ) {
            guard !data.isEmpty else { return }

            do {
                try fileHandle.write(contentsOf: data)
                bytesDownloaded.withLock { $0 += Int64(data.count) }
                progress.completedUnitCount = bytesDownloaded.withLock { $0 }
                progressHandler(progress)
            } catch {
                self.error.withLock { $0 = error }
                dataTask.cancel()
            }
        }

        func urlSession(
            _ session: URLSession,
            task: URLSessionTask,
            didCompleteWithError error: Error?
        ) {
            defer { continuation.withLock { $0 = nil } }

            let finalError = error ?? self.error.withLock { $0 }
            if let finalError {
                continuation.withLock { $0?.resume(throwing: finalError) }
                return
            }

            progress.completedUnitCount = bytesDownloaded.withLock { $0 }
            progressHandler(progress)
            continuation.withLock { $0?.resume(returning: ()) }
        }
    }

    // Fire GET request via delegate-driven streaming session
    var delegate: WindowsDownloadDelegate?
    FileManager.default.createFile(atPath: path, contents: nil)
    let fileHandle = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
    let request = URLRequest(url: url)

    defer {
        try? fileHandle.close()
    }

    try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
        let downloadDelegate = WindowsDownloadDelegate(
            expectedSize: expectedSize,
            fileHandle: fileHandle,
            progressHandler: progressHandler
        )
        delegate = downloadDelegate
        delegate?.continuation.withLock { $0 = continuation }

        let session = URLSession(configuration: .default, delegate: downloadDelegate, delegateQueue: nil)
        let task = session.dataTask(with: request)
        task.resume()
    }
    #endif
}