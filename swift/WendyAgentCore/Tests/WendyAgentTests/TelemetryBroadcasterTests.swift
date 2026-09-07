import Foundation
import GRPCCore
import OpenTelemetryGRPC
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("Telemetry broadcaster")
struct TelemetryBroadcasterTests {
    @Test("log subscriptions separate recent history from live records")
    func subscriptionsSeparateRecentHistoryFromLiveRecords() async {
        let broadcaster = TelemetryBroadcaster()
        await broadcaster.broadcastLogs(logRequest(marker: "stale"))

        let (_, recent, stream) = await broadcaster.subscribeLogs()
        await broadcaster.broadcastLogs(logRequest(marker: "live"))

        #expect(recent.first?.resourceLogs.first?.schemaURL == "stale")
        var iterator = stream.makeAsyncIterator()
        let first = await iterator.next()
        #expect(first?.resourceLogs.first?.schemaURL == "live")
    }

    @Test("canceling a log stream removes its broadcaster subscription", .timeLimit(.minutes(1)))
    func cancelingLogStreamRemovesSubscription() async throws {
        let broadcaster = TelemetryBroadcaster()
        let service = TelemetryService(broadcaster: broadcaster)
        let request = Wendy_Agent_Services_V1_StreamLogsRequest()
        let signalingWriter = SignalingTelemetryWriter()
        var responses = signalingWriter.events.makeAsyncIterator()
        let writer = RPCWriter(wrapping: signalingWriter)
        let streamTask = Task {
            try await service.streamLogs(
                request: request,
                response: writer,
                context: makeTelemetryServerContext(method: "StreamLogs")
            )
        }

        await broadcaster.broadcastLogs(logRequest(marker: "subscription-ready"))
        #expect(await responses.next() != nil)
        streamTask.cancel()
        try await streamTask.value
        #expect(await broadcaster.logSubscriberCountForTesting() == 0)
    }

    @Test(
        "a quiet stream sends empty heartbeats, then delivers data and cleans up",
        .timeLimit(.minutes(1))
    )
    func quietHeartbeats() async throws {
        let broadcaster = TelemetryBroadcaster()
        let service = TelemetryService(
            broadcaster: broadcaster,
            logHeartbeatInterval: .milliseconds(10)
        )
        let writer = SignalingTelemetryWriter()
        var responses = writer.events.makeAsyncIterator()
        let task = Task {
            try await service.streamLogs(
                request: Wendy_Agent_Services_V1_StreamLogsRequest(),
                response: RPCWriter(wrapping: writer),
                context: makeTelemetryServerContext(method: "StreamLogs")
            )
        }
        let heartbeat = try #require(await responses.next())
        #expect(!heartbeat.hasLogs)
        #expect(!heartbeat.isHistory)
        await broadcaster.broadcastLogs(logRequest(marker: "after-silence"))
        while let response = await responses.next() {
            if response.hasLogs {
                #expect(response.logs.resourceLogs.first?.schemaURL == "after-silence")
                #expect(!response.isHistory)
                break
            }
        }
        task.cancel()
        try await task.value
        #expect(await broadcaster.logSubscriberCountForTesting() == 0)
    }

    @Test("heartbeat write failure propagates and unsubscribes", .timeLimit(.minutes(1)))
    func heartbeatFailure() async {
        let broadcaster = TelemetryBroadcaster()
        let service = TelemetryService(
            broadcaster: broadcaster,
            logHeartbeatInterval: .milliseconds(10)
        )
        do {
            try await service.streamLogs(
                request: Wendy_Agent_Services_V1_StreamLogsRequest(),
                response: RPCWriter(wrapping: FailingTelemetryWriter()),
                context: makeTelemetryServerContext(method: "StreamLogs")
            )
            Issue.record("expected write failure")
        } catch {
            #expect(error is RPCError)
        }
        #expect(await broadcaster.logSubscriberCountForTesting() == 0)
    }

    private func logRequest(
        marker: String
    ) -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest {
        var resourceLogs = Opentelemetry_Proto_Logs_V1_ResourceLogs()
        resourceLogs.schemaURL = marker
        var request = Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest()
        request.resourceLogs = [resourceLogs]
        return request
    }
}

private final class SignalingTelemetryWriter: RPCWriterProtocol, @unchecked Sendable {
    typealias Element = Wendy_Agent_Services_V1_StreamLogsResponse

    let events: AsyncStream<Element>
    private let continuation: AsyncStream<Element>.Continuation

    init() {
        (self.events, self.continuation) = AsyncStream.makeStream(
            of: Element.self,
            bufferingPolicy: .bufferingNewest(4)
        )
    }

    func write(_ element: Element) async throws {
        self.continuation.yield(element)
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        for element in elements {
            self.continuation.yield(element)
        }
    }
}

private func makeTelemetryServerContext(method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyTelemetryService",
            method: method
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}

private struct FailingTelemetryWriter: RPCWriterProtocol {
    typealias Element = Wendy_Agent_Services_V1_StreamLogsResponse
    func write(_ element: Element) async throws {
        throw RPCError(code: .unavailable, message: "connection lost")
    }
    func write(contentsOf elements: some Sequence<Element>) async throws {
        throw RPCError(code: .unavailable, message: "connection lost")
    }
}
