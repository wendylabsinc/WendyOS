import Foundation

/// Per-request ephemeral sessions retain neither credentials nor response data
/// beyond the bounded result. All callbacks serialize through this lock.
final class BoundedRequest: NSObject, URLSessionDataDelegate, @unchecked Sendable {
    private let lock = NSLock()
    private let limit: Int
    private let resourceTimeout: TimeInterval
    private var bytes = Data()
    private var failure: (any Error)?
    private var continuation: CheckedContinuation<Data, any Error>?
    private var task: URLSessionDataTask?
    private var session: URLSession?
    private var cancelled = false

    init(limit: Int, resourceTimeout: TimeInterval = 30) { self.limit = limit; self.resourceTimeout = max(0.001, min(30, resourceTimeout)) }

    func run(_ request: URLRequest) async throws -> Data {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                lock.lock()
                if cancelled {
                    lock.unlock()
                    continuation.resume(throwing: CancellationError())
                    return
                }
                self.continuation = continuation
                let configuration = URLSessionConfiguration.ephemeral
                configuration.urlCache = nil
                configuration.httpCookieStorage = nil
                configuration.urlCredentialStorage = nil
                configuration.httpShouldSetCookies = false
                configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
                configuration.timeoutIntervalForRequest = min(15, resourceTimeout)
                configuration.timeoutIntervalForResource = resourceTimeout
                configuration.httpMaximumConnectionsPerHost = 2
                let session = URLSession(configuration: configuration, delegate: self, delegateQueue: nil)
                self.session = session
                let task = session.dataTask(with: request)
                self.task = task
                lock.unlock()
                task.resume()
            }
        } onCancel: { self.cancel() }
    }

    func cancel() {
        lock.lock()
        cancelled = true
        if failure == nil { failure = CancellationError() }
        let task = self.task
        lock.unlock()
        task?.cancel()
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse, completionHandler: @escaping @Sendable (URLSession.ResponseDisposition) -> Void) {
        lock.lock()
        if failure != nil {
            // Preserve a redirect refusal or cancellation already recorded.
        } else if let http = response as? HTTPURLResponse {
            if !(200...299).contains(http.statusCode) { failure = WorldClientError.httpStatus(http.statusCode) }
            else if response.expectedContentLength > Int64(limit) { failure = WorldClientError.responseTooLarge }
        } else { failure = WorldClientError.invalidResponse }
        let allowed = failure == nil
        lock.unlock()
        completionHandler(allowed ? .allow : .cancel)
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        lock.lock()
        if data.count > limit - bytes.count {
            failure = WorldClientError.responseTooLarge
            lock.unlock()
            dataTask.cancel()
            return
        }
        if failure == nil { bytes.append(data) }
        lock.unlock()
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void) {
        lock.lock(); failure = WorldClientError.redirectRefused; lock.unlock()
        completionHandler(nil)
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: (any Error)?) {
        lock.lock()
        let continuation = self.continuation
        self.continuation = nil
        let result: Result<Data, any Error> = if let failure = failure ?? error { .failure(failure) } else { .success(bytes) }
        self.task = nil; self.session = nil; bytes = Data()
        lock.unlock()
        session.finishTasksAndInvalidate()
        continuation?.resume(with: result)
    }
}
