import Foundation
import Testing

@testable import Analytics

// MARK: - Event Name Sanitization

@Suite("GA4 Event Name Sanitization")
struct GA4EventNameSanitizationTests {

    let client = GA4Client(apiSecret: "test-secret", measurementId: "G-TEST123")

    @Test("Valid event names pass through unchanged")
    func validNamesUnchanged() async {
        let result = await client.sanitizeEventName("command_executed")
        #expect(result == "command_executed")
    }

    @Test("Hyphens and spaces are replaced with underscores")
    func specialCharsReplaced() async {
        let result = await client.sanitizeEventName("my-event name")
        #expect(result == "my_event_name")
    }

    @Test("Names starting with a number get e_ prefix")
    func numberPrefixHandled() async {
        let result = await client.sanitizeEventName("123_event")
        #expect(result == "e_123_event")
    }

    @Test("Names starting with underscore get e_ prefix")
    func underscorePrefixHandled() async {
        let result = await client.sanitizeEventName("_private_event")
        #expect(result == "e__private_event")
    }

    @Test("Names are truncated to 40 characters")
    func longNamesTruncated() async {
        let longName = String(repeating: "a", count: 50)
        let result = await client.sanitizeEventName(longName)
        #expect(result.count == 40)
    }

    @Test("Truncation happens after prefix addition")
    func truncationAfterPrefix() async {
        let longName = "1" + String(repeating: "a", count: 50)
        let result = await client.sanitizeEventName(longName)
        #expect(result.count == 40)
        #expect(result.hasPrefix("e_"))
    }
}

// MARK: - Parameter Name Sanitization

@Suite("GA4 Parameter Name Sanitization")
struct GA4ParamNameSanitizationTests {

    let client = GA4Client(apiSecret: "test-secret", measurementId: "G-TEST123")

    @Test("Valid param names pass through unchanged")
    func validNamesUnchanged() async {
        let result = await client.sanitizeParamName("command_name")
        #expect(result == "command_name")
    }

    @Test("Special characters are replaced with underscores")
    func specialCharsReplaced() async {
        let result = await client.sanitizeParamName("param-name.here")
        #expect(result == "param_name_here")
    }

    @Test("Param names are truncated to 40 characters")
    func longNamesTruncated() async {
        let longName = String(repeating: "x", count: 50)
        let result = await client.sanitizeParamName(longName)
        #expect(result.count == 40)
    }
}

// MARK: - Parameter Value Truncation

@Suite("GA4 Parameter Value Truncation")
struct GA4ParamValueTruncationTests {

    let client = GA4Client(apiSecret: "test-secret", measurementId: "G-TEST123")

    @Test("Short values pass through unchanged")
    func shortValuesUnchanged() async {
        let result = await client.truncateParamValue("short")
        #expect(result == "short")
    }

    @Test("Values at exactly 100 characters are not truncated")
    func exactLimitUnchanged() async {
        let value = String(repeating: "a", count: 100)
        let result = await client.truncateParamValue(value)
        #expect(result.count == 100)
    }

    @Test("Values over 100 characters are truncated")
    func longValuesTruncated() async {
        let value = String(repeating: "b", count: 150)
        let result = await client.truncateParamValue(value)
        #expect(result.count == 100)
    }
}

// MARK: - Timestamp Conversion

@Suite("GA4 Timestamp Conversion")
struct GA4TimestampConversionTests {

    let client = GA4Client(apiSecret: "test-secret", measurementId: "G-TEST123")

    @Test("ISO8601 timestamp converts to microseconds")
    func validTimestampConversion() async {
        // 2024-01-01T00:00:00Z = 1704067200 seconds = 1704067200000000 microseconds
        let result = await client.toMicroseconds("2024-01-01T00:00:00Z")
        #expect(result == "1704067200000000")
    }

    @Test("Invalid timestamp falls back to current time")
    func invalidTimestampFallback() async {
        let before = Int64(Date().timeIntervalSince1970 * 1_000_000)
        let result = await client.toMicroseconds("not-a-timestamp")
        let after = Int64(Date().timeIntervalSince1970 * 1_000_000)

        let resultValue = Int64(result)!
        #expect(resultValue >= before)
        #expect(resultValue <= after)
    }
}

// MARK: - Payload Structure

@Suite("GA4 Payload Structure")
struct GA4PayloadStructureTests {

    let client = GA4Client(apiSecret: "test-secret", measurementId: "G-TEST123")

    @Test("Payload contains required GA4 fields")
    func payloadHasRequiredFields() async throws {
        let event = AnalyticsEvent(
            name: "test_event",
            properties: ["key": "value"],
            distinctId: "test-user-id"
        )

        let data = await client.buildPayload(events: [event])
        let json = try #require(
            data.flatMap {
                try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
            }
        )

        #expect(json["client_id"] as? String == "test-user-id")
        #expect(json["timestamp_micros"] is String)

        let events = try #require(json["events"] as? [[String: Any]])
        #expect(events.count == 1)
        #expect(events[0]["name"] as? String == "test_event")

        let params = try #require(events[0]["params"] as? [String: String])
        #expect(params["key"] == "value")
        #expect(params["engagement_time_msec"] == "100")
    }

    @Test("Multiple events are included in single payload")
    func multipleEventsInPayload() async throws {
        let events = (0..<5).map { i in
            AnalyticsEvent(
                name: "event_\(i)",
                properties: ["index": "\(i)"],
                distinctId: "test-user-id"
            )
        }

        let data = await client.buildPayload(events: events)
        let json = try #require(
            data.flatMap {
                try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
            }
        )

        let ga4Events = try #require(json["events"] as? [[String: Any]])
        #expect(ga4Events.count == 5)
    }

    @Test("Engagement time is not overwritten if already present")
    func existingEngagementTimePreserved() async throws {
        let event = AnalyticsEvent(
            name: "test_event",
            properties: ["engagement_time_msec": "500"],
            distinctId: "test-user-id"
        )

        let data = await client.buildPayload(events: [event])
        let json = try #require(
            data.flatMap {
                try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
            }
        )

        let events = try #require(json["events"] as? [[String: Any]])
        let params = try #require(events[0]["params"] as? [String: String])
        #expect(params["engagement_time_msec"] == "500")
    }

    @Test("Empty events array returns nil")
    func emptyEventsReturnsNil() async {
        let result = await client.buildPayload(events: [])
        #expect(result == nil)
    }
}

// MARK: - AnalyticsEvent

@Suite("AnalyticsEvent")
struct AnalyticsEventTests {

    @Test("Event initializes with correct values")
    func eventInitialization() {
        let event = AnalyticsEvent(
            name: "test_event",
            properties: ["key": "value"],
            distinctId: "user-123"
        )

        #expect(event.event == "test_event")
        #expect(event.properties["key"] == "value")
        #expect(event.distinctId == "user-123")
        #expect(!event.timestamp.isEmpty)
    }

    @Test("Event timestamp is valid ISO8601")
    func eventTimestampFormat() {
        let event = AnalyticsEvent(
            name: "test",
            properties: [:],
            distinctId: "user"
        )

        let formatter = ISO8601DateFormatter()
        let parsed = formatter.date(from: event.timestamp)
        #expect(parsed != nil)
    }
}
