import Foundation
import Logging

#if os(Windows)
    import FoundationNetworking
#else
    import AsyncHTTPClient
#endif

/// Represents an analytics event to be tracked
public struct AnalyticsEvent: Sendable, Encodable {
    /// The event name
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
}

/// Client for sending analytics events to Google Analytics 4 via the Measurement Protocol
public actor GA4Client {
    private let apiSecret: String
    private let measurementId: String
    private let logger = Logger(label: "sh.wendy.analytics.ga4")

    /// Queue of events to be sent
    private var eventQueue: [AnalyticsEvent] = []

    /// Maximum number of events per GA4 request
    /// G4A has a 25 event limit per patch
    private let maxBatchSize = 25

    /// Maximum time to wait before sending a batch (in seconds)
    private let batchInterval: TimeInterval = 30

    /// Timer for batch sending
    private var batchTimer: Task<Void, Never>?

    /// Initialize the GA4 client
    public init(
        apiSecret: String,
        measurementId: String
    ) {
        self.apiSecret = apiSecret
        self.measurementId = measurementId
    }

    deinit {
        batchTimer?.cancel()
    }

    /// Captures an analytics event
    public func capture(event: AnalyticsEvent) async {
        eventQueue.append(event)

        if eventQueue.count >= maxBatchSize {
            await sendBatch()
        } else if batchTimer == nil {
            startBatchTimer()
        }
    }

    /// Sends a single event immediately (bypassing the queue)
    public func captureImmediate(event: AnalyticsEvent) async {
        await sendEvents([event])
    }

    /// Flushes all pending events immediately
    public func flush() async {
        await sendBatch()
    }

    /// Sends all queued events as a batch
    private func sendBatch() async {
        guard !eventQueue.isEmpty else { return }

        let events = eventQueue
        eventQueue = []

        batchTimer?.cancel()
        batchTimer = nil

        // GA4 allows max 25 events per request; chunk if needed
        let chunks = stride(from: 0, to: events.count, by: maxBatchSize).map {
            Array(events[$0..<min($0 + maxBatchSize, events.count)])
        }

        for chunk in chunks {
            await sendEvents(chunk)
        }
    }

    /// Sends a chunk of events to the GA4 Measurement Protocol endpoint
    private func sendEvents(_ events: [AnalyticsEvent]) async {
        guard let body = buildPayload(events: events) else { return }

        let url = buildURL()

        do {
            #if os(macOS) || os(Windows)
                var request = URLRequest(url: URL(string: url)!)
                request.httpMethod = "POST"
                request.allHTTPHeaderFields = ["Content-Type": "application/json"]
                request.httpBody = body
                let (_, response) = try await URLSession.shared.data(for: request)
                if let response = response as? HTTPURLResponse, response.statusCode >= 400 {
                    logger.debug("GA4 send failed with status: \(response.statusCode)")
                }
            #else
                var request = HTTPClientRequest(url: url)
                request.method = .POST
                request.headers.add(name: "Content-Type", value: "application/json")
                request.body = .bytes(body)
                let response = try await HTTPClient.shared.execute(request, timeout: .seconds(10))
                if response.status.code >= 400 {
                    logger.debug("GA4 send failed with status: \(response.status)")
                }
            #endif
        } catch {
            // Fail silently - we don't want analytics errors to break the CLI
            logger.debug("Failed to send analytics events: \(error)")
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

    // MARK: - Payload Construction

    private func buildURL() -> String {
        "https://www.google-analytics.com/mp/collect?api_secret=\(apiSecret)&measurement_id=\(measurementId)"
    }

    /// Builds the GA4 Measurement Protocol JSON payload from analytics events
    func buildPayload(events: [AnalyticsEvent]) -> Data? {
        guard let firstEvent = events.first else { return nil }

        let ga4Events: [GA4Event] = events.map { event in
            var params: [String: String] = [:]
            for (key, value) in event.properties {
                params[sanitizeParamName(key)] = truncateParamValue(value)
            }
            // GA4 requires engagement_time_msec for user engagement metrics
            if params["engagement_time_msec"] == nil {
                params["engagement_time_msec"] = "100"
            }
            return GA4Event(
                name: sanitizeEventName(event.event),
                params: params
            )
        }

        let payload = GA4Payload(
            clientId: firstEvent.distinctId,
            timestampMicros: toMicroseconds(firstEvent.timestamp),
            events: ga4Events
        )

        return try? JSONEncoder().encode(payload)
    }

    // MARK: - GA4 Compliance Helpers

    /// Converts an ISO8601 timestamp string to microseconds since epoch
    func toMicroseconds(_ iso8601: String) -> String {
        let formatter = ISO8601DateFormatter()
        guard let date = formatter.date(from: iso8601) else {
            return String(Int64(Date().timeIntervalSince1970 * 1_000_000))
        }
        return String(Int64(date.timeIntervalSince1970 * 1_000_000))
    }

    /// Sanitizes an event name to comply with GA4 constraints:
    /// 40 chars max, alphanumeric + underscores, must start with a letter
    func sanitizeEventName(_ name: String) -> String {
        var sanitized = name.replacingOccurrences(
            of: "[^a-zA-Z0-9_]",
            with: "_",
            options: .regularExpression
        )
        if let first = sanitized.first, !first.isLetter {
            sanitized = "e_" + sanitized
        }
        return String(sanitized.prefix(40))
    }

    /// Sanitizes a parameter name: 40 chars max, alphanumeric + underscores
    func sanitizeParamName(_ name: String) -> String {
        let sanitized = name.replacingOccurrences(
            of: "[^a-zA-Z0-9_]",
            with: "_",
            options: .regularExpression
        )
        return String(sanitized.prefix(40))
    }

    /// Truncates a parameter value to GA4's 100-character limit
    func truncateParamValue(_ value: String) -> String {
        String(value.prefix(100))
    }
}

// MARK: - GA4 Payload Types

/// Top-level GA4 Measurement Protocol request body
struct GA4Payload: Codable, Sendable {
    let clientId: String
    let timestampMicros: String
    let events: [GA4Event]

    enum CodingKeys: String, CodingKey {
        case clientId = "client_id"
        case timestampMicros = "timestamp_micros"
        case events
    }
}

/// A single event within a GA4 Measurement Protocol request
struct GA4Event: Codable, Sendable {
    let name: String
    let params: [String: String]
}
