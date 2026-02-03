import Foundation
import Hummingbird
import Logging
import NIOCore
import OpenTelemetryGRPC

/// OTLP/HTTP receiver that accepts telemetry on port 4318.
/// Supports protobuf-encoded logs, metrics, and traces.
struct OTLPHTTPReceiver {
    let broadcaster: TelemetryBroadcaster
    let cloudClient: CloudClient?
    let logger = Logger(label: "sh.wendy.agent.otlp-http")

    init(broadcaster: TelemetryBroadcaster, cloudClient: CloudClient? = nil) {
        self.broadcaster = broadcaster
        self.cloudClient = cloudClient
    }

    func buildApplication() -> some ApplicationProtocol {
        let router = Router()

        // POST /v1/logs - Receive logs
        router.post("/v1/logs") { request, context in
            try await handleLogs(request: request, context: context)
        }

        // POST /v1/metrics - Receive metrics
        router.post("/v1/metrics") { request, context in
            try await handleMetrics(request: request, context: context)
        }

        // POST /v1/traces - Receive traces (broadcast only, no cloud forwarding yet)
        router.post("/v1/traces") { request, context in
            try await handleTraces(request: request, context: context)
        }

        let app = Application(
            router: router,
            configuration: .init(
                address: .hostname("127.0.0.1", port: 4318)
            )
        )

        return app
    }

    private func handleLogs(
        request: Request,
        context: some RequestContext
    ) async throws -> Response {
        let body = try await request.body.collect(upTo: 10 * 1024 * 1024)  // 10MB limit
        let data = Data(buffer: body)

        do {
            let logsRequest = try Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest(
                serializedBytes: data
            )

            // Broadcast to CLI subscribers
            await broadcaster.broadcastLogs(logsRequest)

            // Forward to cloud if enrolled
            if let cloud = cloudClient {
                do {
                    let otel = await Opentelemetry_Proto_Collector_Logs_V1_LogsService.Client(
                        wrapping: cloud.grpcClient
                    )
                    _ = try await otel.export(logsRequest)
                } catch {
                    logger.error("Failed to forward logs to cloud: \(error)")
                }
            }

            let response = Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceResponse()
            return Response(
                status: .ok,
                headers: [.contentType: "application/x-protobuf"],
                body: .init(byteBuffer: ByteBuffer(data: try response.serializedData()))
            )
        } catch {
            logger.error("Failed to parse logs request: \(error)")
            return Response(status: .badRequest)
        }
    }

    private func handleMetrics(
        request: Request,
        context: some RequestContext
    ) async throws -> Response {
        let body = try await request.body.collect(upTo: 10 * 1024 * 1024)
        let data = Data(buffer: body)

        do {
            let metricsRequest =
                try Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest(
                    serializedBytes: data
                )

            // Broadcast to CLI subscribers
            await broadcaster.broadcastMetrics(metricsRequest)

            // Forward to cloud if enrolled
            if let cloud = cloudClient {
                do {
                    let otel = await Opentelemetry_Proto_Collector_Metrics_V1_MetricsService.Client(
                        wrapping: cloud.grpcClient
                    )
                    _ = try await otel.export(metricsRequest)
                } catch {
                    logger.error("Failed to forward metrics to cloud: \(error)")
                }
            }

            let response = Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceResponse()
            return Response(
                status: .ok,
                headers: [.contentType: "application/x-protobuf"],
                body: .init(byteBuffer: ByteBuffer(data: try response.serializedData()))
            )
        } catch {
            logger.error("Failed to parse metrics request: \(error)")
            return Response(status: .badRequest)
        }
    }

    private func handleTraces(
        request: Request,
        context: some RequestContext
    ) async throws -> Response {
        let body = try await request.body.collect(upTo: 10 * 1024 * 1024)
        let data = Data(buffer: body)

        do {
            let tracesRequest = try Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest(
                serializedBytes: data
            )

            // Broadcast to CLI subscribers
            await broadcaster.broadcastTraces(tracesRequest)

            // TODO: Forward to cloud when trace support is added

            let response = Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceResponse()
            return Response(
                status: .ok,
                headers: [.contentType: "application/x-protobuf"],
                body: .init(byteBuffer: ByteBuffer(data: try response.serializedData()))
            )
        } catch {
            logger.error("Failed to parse traces request: \(error)")
            return Response(status: .badRequest)
        }
    }
}
