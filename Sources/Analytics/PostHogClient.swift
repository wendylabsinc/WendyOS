import AsyncHTTPClient
import Foundation
import Logging
import NIOFoundationCompat

/// Represents an analytics event to be tracked
public struct AnalyticsEvent: Sendable, Encodable {
    /// The event name (e.g., "command_executed", "command_failed")
    public let event: String

    /// Properties associated with the event
    public let properties: [String: String]

    /// Unique identifier for the user (anonymous UUID)
    public let distinctId: String

    /// Timestamp of the event
    public let timestamp: String

    public init(
        name: String,
        properties: [String: String],
        distinctId: String,
        timestamp: Date = Date()
    ) {
        self.event = name
        self.properties = properties
        self.distinctId = distinctId
        self.timestamp = ISO8601DateFormatter().string(from: timestamp)
    }

    // Custom encoding to match PostHog's expected format
    enum CodingKeys: String, CodingKey {
        case event
        case properties
        case distinctId = "distinct_id"
        case timestamp
    }
}

/// Batch of events for sending to PostHog
public struct AnalyticsBatch: Sendable, Encodable {
    public let apiKey: String
    public let batch: [AnalyticsEvent]

    enum CodingKeys: String, CodingKey {
        case apiKey = "api_key"
        case batch
    }
}

/// Client for sending analytics events to PostHog
public actor PostHogClient {
    private let apiKey: String
    private let host: String
    private let httpClient: HTTPClient
    private let logger = Logger(label: "sh.wendy.analytics.posthog")

    /// Queue of events to be sent
    private var eventQueue: [AnalyticsEvent] = []

    /// Maximum number of events to batch
    private let maxBatchSize = 100

    /// Maximum time to wait before sending a batch (in seconds)
    private let batchInterval: TimeInterval = 30

    /// Timer for batch sending
    private var batchTimer: Task<Void, Never>?

    /// Initialize the PostHog client
    public init(
        apiKey: String,
        host: String = "https://app.posthog.com",
        httpClient: HTTPClient? = nil
    ) {
        self.apiKey = apiKey
        self.host = host
        self.httpClient = httpClient ?? HTTPClient.shared
    }

    deinit {
        batchTimer?.cancel()
    }

    /// Captures an analytics event
    public func capture(event: AnalyticsEvent) async {
        // Add to queue
        eventQueue.append(event)

        // Send immediately if we've reached max batch size
        if eventQueue.count >= maxBatchSize {
            await sendBatch()
        } else if batchTimer == nil {
            // Start batch timer if not already running
            startBatchTimer()
        }
    }

    /// Sends a single event immediately (bypassing the queue)
    public func captureImmediate(event: AnalyticsEvent) async {
        do {
            let url = "\(host)/capture/"
            var request = HTTPClientRequest(url: url)
            request.method = .POST
            request.headers.add(name: "Content-Type", value: "application/json")

            let encoder = JSONEncoder()
            let data = try encoder.encode(event)
            request.body = .bytes(data)

            let response = try await httpClient.execute(request, timeout: .seconds(10))

            if response.status.code >= 400 {
                logger.debug("PostHog capture failed with status: \(response.status)")
            }
        } catch {
            // Fail silently - we don't want analytics errors to break the CLI
            logger.debug("Failed to send analytics event: \(error)")
        }
    }

    /// Sends all queued events as a batch
    private func sendBatch() async {
        guard !eventQueue.isEmpty else { return }

        // Take current queue and clear it
        let events = eventQueue
        eventQueue = []

        // Cancel batch timer since we're sending now
        batchTimer?.cancel()
        batchTimer = nil

        do {
            let url = "\(host)/batch/"
            var request = HTTPClientRequest(url: url)
            request.method = .POST
            request.headers.add(name: "Content-Type", value: "application/json")

            let batch = AnalyticsBatch(apiKey: apiKey, batch: events)
            let encoder = JSONEncoder()
            let data = try encoder.encode(batch)
            request.body = .bytes(data)

            let response = try await httpClient.execute(request, timeout: .seconds(10))

            if response.status.code >= 400 {
                logger.debug("PostHog batch send failed with status: \(response.status)")
            } else {
                logger.debug("Successfully sent \(events.count) events to PostHog")
            }
        } catch {
            // Fail silently - we don't want analytics errors to break the CLI
            logger.debug("Failed to send analytics batch: \(error)")
        }
    }

    /// Starts a timer to send batched events after an interval
    private func startBatchTimer() {
        batchTimer?.cancel()
        batchTimer = Task {
            try? await Task.sleep(nanoseconds: UInt64(batchInterval * 1_000_000_000))
            await sendBatch()
        }
    }

    /// Flushes all pending events immediately
    public func flush() async {
        await sendBatch()
    }
}

/// Errors that can occur with PostHog client
public enum PostHogError: Error {
    case networkError(Error)
    case invalidResponse

    public var localizedDescription: String {
        switch self {
        case .networkError(let error):
            return "Network error: \(error)"
        case .invalidResponse:
            return "Invalid response from PostHog"
        }
    }
}
