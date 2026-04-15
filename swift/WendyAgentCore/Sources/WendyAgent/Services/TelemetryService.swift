import GRPCCore
import Logging
import OpenTelemetryGRPC
import WendyAgentGRPC

/// Service that streams telemetry data to CLI clients.
/// Subscribes to the TelemetryBroadcaster and streams logs/metrics to connected clients.
actor TelemetryService: Wendy_Agent_Services_V1_WendyTelemetryService.SimpleServiceProtocol
{
    let broadcaster: TelemetryBroadcaster
    let logger = Logger(label: "sh.wendy.agent.telemetry-streaming")

    init(broadcaster: TelemetryBroadcaster) {
        self.broadcaster = broadcaster
    }

    func streamLogs(
        request: Wendy_Agent_Services_V1_StreamLogsRequest,
        response: RPCWriter<Wendy_Agent_Services_V1_StreamLogsResponse>,
        context: ServerContext
    ) async throws {
        let serviceFilter = request.hasServiceName ? "\(request.serviceName)" : "none"
        let minSeverity = request.hasMinSeverity ? "\(request.minSeverity)" : "none"

        logger.info(
            "Client subscribed to log stream",
            metadata: [
                "service_filter": .string(serviceFilter),
                "min_severity": .string(minSeverity),
            ]
        )

        let (subscriptionId, stream) = await broadcaster.subscribeLogs()
        defer {
            Task {
                await broadcaster.unsubscribeLogs(id: subscriptionId)
            }
        }

        for await logsRequest in stream {
            // Apply filters if specified
            let filteredRequest = filterLogs(
                logsRequest,
                serviceName: request.hasServiceName ? request.serviceName : nil,
                minSeverity: request.hasMinSeverity ? request.minSeverity : nil,
                appName: request.hasAppName ? request.appName : nil
            )

            // Only send if there are logs after filtering
            if !filteredRequest.resourceLogs.isEmpty {
                try await response.write(
                    Wendy_Agent_Services_V1_StreamLogsResponse.with {
                        $0.logs = filteredRequest
                    }
                )
            }
        }

        logger.info("Client disconnected from log stream")
    }

    func streamMetrics(
        request: Wendy_Agent_Services_V1_StreamMetricsRequest,
        response: RPCWriter<Wendy_Agent_Services_V1_StreamMetricsResponse>,
        context: ServerContext
    ) async throws {
        let metricServiceFilter = request.hasServiceName ? "\(request.serviceName)" : "none"
        let metricPrefix = request.hasMetricNamePrefix ? "\(request.metricNamePrefix)" : "none"

        logger.info(
            "Client subscribed to metrics stream",
            metadata: [
                "service_filter": .string(metricServiceFilter),
                "metric_prefix": .string(metricPrefix),
            ]
        )

        let (subscriptionId, stream) = await broadcaster.subscribeMetrics()
        defer {
            Task {
                await broadcaster.unsubscribeMetrics(id: subscriptionId)
            }
        }

        for await metricsRequest in stream {
            // Apply filters if specified
            let filteredRequest = filterMetrics(
                metricsRequest,
                serviceName: request.hasServiceName ? request.serviceName : nil,
                metricNamePrefix: request.hasMetricNamePrefix ? request.metricNamePrefix : nil,
                appName: request.hasAppName ? request.appName : nil
            )

            // Only send if there are metrics after filtering
            if !filteredRequest.resourceMetrics.isEmpty {
                try await response.write(
                    Wendy_Agent_Services_V1_StreamMetricsResponse.with {
                        $0.metrics = filteredRequest
                    }
                )
            }
        }

        logger.info("Client disconnected from metrics stream")
    }

    func streamTraces(
        request: Wendy_Agent_Services_V1_StreamTracesRequest,
        response: RPCWriter<Wendy_Agent_Services_V1_StreamTracesResponse>,
        context: ServerContext
    ) async throws {
        let serviceFilter = request.hasServiceName ? "\(request.serviceName)" : "none"
        let spanPrefix = request.hasSpanNamePrefix ? "\(request.spanNamePrefix)" : "none"

        logger.info(
            "Client subscribed to traces stream",
            metadata: [
                "service_filter": .string(serviceFilter),
                "span_prefix": .string(spanPrefix),
            ]
        )

        let (subscriptionId, stream) = await broadcaster.subscribeTraces()
        defer {
            Task {
                await broadcaster.unsubscribeTraces(id: subscriptionId)
            }
        }

        for await tracesRequest in stream {
            // Apply filters if specified
            let filteredRequest = filterTraces(
                tracesRequest,
                serviceName: request.hasServiceName ? request.serviceName : nil,
                appName: request.hasAppName ? request.appName : nil,
                spanNamePrefix: request.hasSpanNamePrefix ? request.spanNamePrefix : nil
            )

            // Only send if there are traces after filtering
            if !filteredRequest.resourceSpans.isEmpty {
                try await response.write(
                    Wendy_Agent_Services_V1_StreamTracesResponse.with {
                        $0.traces = filteredRequest
                    }
                )
            }
        }

        logger.info("Client disconnected from traces stream")
    }

    // MARK: - Filtering

    private func filterLogs(
        _ request: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest,
        serviceName: String?,
        minSeverity: Int32?,
        appName: String?
    ) -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest {
        var filtered = request

        if let serviceName = serviceName {
            filtered.resourceLogs = filtered.resourceLogs.filter { resourceLogs in
                // Check if service.name attribute matches
                resourceLogs.resource.attributes.contains { attr in
                    attr.key == "service.name" && attr.value.stringValue == serviceName
                }
            }
        }

        if let appName = appName {
            filtered.resourceLogs = filtered.resourceLogs.filter { resourceLogs in
                // Check if wendy.app.name attribute matches
                resourceLogs.resource.attributes.contains { attr in
                    attr.key == "wendy.app.name" && attr.value.stringValue == appName
                }
            }
        }

        if let minSeverity = minSeverity {
            filtered.resourceLogs = filtered.resourceLogs.map { resourceLogs in
                var filtered = resourceLogs
                filtered.scopeLogs = filtered.scopeLogs.map { scopeLogs in
                    var filtered = scopeLogs
                    filtered.logRecords = filtered.logRecords.filter { record in
                        record.severityNumber.rawValue >= minSeverity
                    }
                    return filtered
                }.filter { !$0.logRecords.isEmpty }
                return filtered
            }.filter { !$0.scopeLogs.isEmpty }
        }

        return filtered
    }

    private func filterMetrics(
        _ request: Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest,
        serviceName: String?,
        metricNamePrefix: String?,
        appName: String?
    ) -> Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest {
        var filtered = request

        if let serviceName = serviceName {
            filtered.resourceMetrics = filtered.resourceMetrics.filter { resourceMetrics in
                // Check if service.name attribute matches
                resourceMetrics.resource.attributes.contains { attr in
                    attr.key == "service.name" && attr.value.stringValue == serviceName
                }
            }
        }

        if let appName = appName {
            filtered.resourceMetrics = filtered.resourceMetrics.filter { resourceMetrics in
                // Check if wendy.app.name attribute matches
                resourceMetrics.resource.attributes.contains { attr in
                    attr.key == "wendy.app.name" && attr.value.stringValue == appName
                }
            }
        }

        if let metricNamePrefix = metricNamePrefix {
            filtered.resourceMetrics = filtered.resourceMetrics.map { resourceMetrics in
                var filtered = resourceMetrics
                filtered.scopeMetrics = filtered.scopeMetrics.map { scopeMetrics in
                    var filtered = scopeMetrics
                    filtered.metrics = filtered.metrics.filter { metric in
                        metric.name.hasPrefix(metricNamePrefix)
                    }
                    return filtered
                }.filter { !$0.metrics.isEmpty }
                return filtered
            }.filter { !$0.scopeMetrics.isEmpty }
        }

        return filtered
    }

    private func filterTraces(
        _ request: Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest,
        serviceName: String?,
        appName: String?,
        spanNamePrefix: String?
    ) -> Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest {
        var filtered = request

        if let serviceName = serviceName {
            filtered.resourceSpans = filtered.resourceSpans.filter { resourceSpans in
                // Check if service.name attribute matches
                resourceSpans.resource.attributes.contains { attr in
                    attr.key == "service.name" && attr.value.stringValue == serviceName
                }
            }
        }

        if let appName = appName {
            filtered.resourceSpans = filtered.resourceSpans.filter { resourceSpans in
                // Check if wendy.app.name attribute matches
                resourceSpans.resource.attributes.contains { attr in
                    attr.key == "wendy.app.name" && attr.value.stringValue == appName
                }
            }
        }

        if let spanNamePrefix = spanNamePrefix {
            filtered.resourceSpans = filtered.resourceSpans.map { resourceSpans in
                var filtered = resourceSpans
                filtered.scopeSpans = filtered.scopeSpans.map { scopeSpans in
                    var filtered = scopeSpans
                    filtered.spans = filtered.spans.filter { span in
                        span.name.hasPrefix(spanNamePrefix)
                    }
                    return filtered
                }.filter { !$0.spans.isEmpty }
                return filtered
            }.filter { !$0.scopeSpans.isEmpty }
        }

        return filtered
    }
}
