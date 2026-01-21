import Logging
import OpenTelemetryGRPC

/// Proxies OTel logs to cloud while broadcasting to CLI clients.
actor OpenTelemetryLogsProxy: Opentelemetry_Proto_Collector_Logs_V1_LogsService
        .SimpleServiceProtocol
{
    let cloud: CloudClient
    let broadcaster: TelemetryBroadcaster
    let logger = Logger(label: "sh.wendy.agent.otel-logs-proxy")

    init(cloud: CloudClient, broadcaster: TelemetryBroadcaster) {
        self.cloud = cloud
        self.broadcaster = broadcaster
    }

    func export(
        request: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest,
        context: ServerContext
    ) async throws -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceResponse {
        // Broadcast to local subscribers (CLI clients)
        await broadcaster.broadcastLogs(request)

        // Forward to cloud
        do {
            let otel = await Opentelemetry_Proto_Collector_Logs_V1_LogsService.Client(
                wrapping: cloud.grpcClient
            )
            return try await otel.export(request)
        } catch {
            logger.error("Error exporting logs: \(error)")
            return Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceResponse()
        }
    }
}

/// Proxies OTel metrics to cloud while broadcasting to CLI clients.
actor OpenTelemetryMetricsProxy: Opentelemetry_Proto_Collector_Metrics_V1_MetricsService
        .SimpleServiceProtocol
{
    let cloud: CloudClient
    let broadcaster: TelemetryBroadcaster
    let logger = Logger(label: "sh.wendy.agent.otel-metrics-proxy")

    init(cloud: CloudClient, broadcaster: TelemetryBroadcaster) {
        self.cloud = cloud
        self.broadcaster = broadcaster
    }

    func export(
        request: Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest,
        context: ServerContext
    ) async throws -> Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceResponse {
        // Broadcast to local subscribers (CLI clients)
        await broadcaster.broadcastMetrics(request)

        // Forward to cloud
        do {
            let otel = await Opentelemetry_Proto_Collector_Metrics_V1_MetricsService.Client(
                wrapping: cloud.grpcClient
            )
            return try await otel.export(request)
        } catch {
            logger.error("Error exporting metrics: \(error)")
            return Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceResponse()
        }
    }
}
