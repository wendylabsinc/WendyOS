import GRPCCore
import Logging
import OpenTelemetryGRPC

/// Local OTel logs receiver that broadcasts to CLI clients without requiring cloud enrollment.
actor LocalOTelLogsReceiver: Opentelemetry_Proto_Collector_Logs_V1_LogsService.SimpleServiceProtocol
{
    let broadcaster: TelemetryBroadcaster
    let logger = Logger(label: "sh.wendy.agent.local-otel-logs")

    init(broadcaster: TelemetryBroadcaster) {
        self.broadcaster = broadcaster
    }

    func export(
        request: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest,
        context: ServerContext
    ) async throws -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceResponse {
        await broadcaster.broadcastLogs(request)
        return Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceResponse()
    }
}

/// Local OTel metrics receiver that broadcasts to CLI clients without requiring cloud enrollment.
actor LocalOTelMetricsReceiver: Opentelemetry_Proto_Collector_Metrics_V1_MetricsService
        .SimpleServiceProtocol
{
    let broadcaster: TelemetryBroadcaster
    let logger = Logger(label: "sh.wendy.agent.local-otel-metrics")

    init(broadcaster: TelemetryBroadcaster) {
        self.broadcaster = broadcaster
    }

    func export(
        request: Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest,
        context: ServerContext
    ) async throws -> Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceResponse {
        await broadcaster.broadcastMetrics(request)
        return Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceResponse()
    }
}

/// Local OTel traces receiver that broadcasts to CLI clients without requiring cloud enrollment.
actor LocalOTelTracesReceiver: Opentelemetry_Proto_Collector_Trace_V1_TraceService
        .SimpleServiceProtocol
{
    let broadcaster: TelemetryBroadcaster
    let logger = Logger(label: "sh.wendy.agent.local-otel-traces")

    init(broadcaster: TelemetryBroadcaster) {
        self.broadcaster = broadcaster
    }

    func export(
        request: Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest,
        context: ServerContext
    ) async throws -> Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceResponse {
        await broadcaster.broadcastTraces(request)
        return Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceResponse()
    }
}
