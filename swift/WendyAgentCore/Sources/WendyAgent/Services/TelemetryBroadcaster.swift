import Foundation
import Logging
import OpenTelemetryGRPC

/// Actor that broadcasts telemetry data to multiple subscribers.
/// Used to fan out logs, metrics, and traces from the OTel proxy to CLI clients.
actor TelemetryBroadcaster {
    typealias LogsRequest = Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest
    typealias MetricsRequest = Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest
    typealias TracesRequest = Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest

    private var logSubscribers: [UUID: AsyncStream<LogsRequest>.Continuation] = [:]
    private var metricsSubscribers: [UUID: AsyncStream<MetricsRequest>.Continuation] = [:]
    private var tracesSubscribers: [UUID: AsyncStream<TracesRequest>.Continuation] = [:]
    private let logger = Logger(label: "sh.wendy.agent.telemetry-broadcaster")

    /// Cache of the latest metrics by metric key (service:metricName)
    /// This allows new subscribers to immediately see current metric values
    private var latestMetrics: [String: Opentelemetry_Proto_Metrics_V1_Metric] = [:]
    private var latestMetricsResources: [String: Opentelemetry_Proto_Resource_V1_Resource] = [:]

    /// Cache of recent log entries for new subscribers
    private var recentLogs: [LogsRequest] = []
    private let maxCachedLogs = 20

    init() {}

    /// Subscribe to log broadcasts. Returns a stream that yields log requests.
    /// Immediately sends cached recent logs if available.
    func subscribeLogs() -> (id: UUID, stream: AsyncStream<LogsRequest>) {
        let id = UUID()
        let (stream, continuation) = AsyncStream<LogsRequest>.makeStream(
            bufferingPolicy: .bufferingNewest(100)
        )
        logSubscribers[id] = continuation
        logger.debug("Log subscriber added", metadata: ["id": "\(id)"])

        // Send cached logs immediately so new subscribers see recent history
        if !recentLogs.isEmpty {
            for cachedLog in recentLogs {
                _ = continuation.yield(cachedLog)
            }
            logger.debug(
                "Sent cached logs to new subscriber",
                metadata: [
                    "id": "\(id)",
                    "logBatches": "\(recentLogs.count)",
                ]
            )
        }

        return (id, stream)
    }

    /// Unsubscribe from log broadcasts.
    func unsubscribeLogs(id: UUID) {
        if let continuation = logSubscribers.removeValue(forKey: id) {
            continuation.finish()
            logger.debug("Log subscriber removed", metadata: ["id": "\(id)"])
        }
    }

    /// Broadcast logs to all subscribers and cache for new subscribers.
    func broadcastLogs(_ request: LogsRequest) {
        // Cache logs for new subscribers
        cacheLogs(request)

        for (id, continuation) in logSubscribers {
            let result = continuation.yield(request)
            if case .terminated = result {
                logSubscribers.removeValue(forKey: id)
            }
        }
    }

    /// Cache log entries, keeping only the most recent batches.
    private func cacheLogs(_ request: LogsRequest) {
        recentLogs.append(request)

        // Trim to keep only recent logs
        if recentLogs.count > maxCachedLogs {
            recentLogs.removeFirst(recentLogs.count - maxCachedLogs)
        }
    }

    /// Subscribe to metrics broadcasts. Returns a stream that yields metrics requests.
    /// Immediately sends the latest cached metrics if available.
    func subscribeMetrics() -> (id: UUID, stream: AsyncStream<MetricsRequest>) {
        let id = UUID()
        let (stream, continuation) = AsyncStream<MetricsRequest>.makeStream(
            bufferingPolicy: .bufferingNewest(100)
        )
        metricsSubscribers[id] = continuation
        logger.debug("Metrics subscriber added", metadata: ["id": "\(id)"])

        // Send cached metrics immediately so new subscribers see current state
        if !latestMetrics.isEmpty {
            let cachedRequest = buildCachedMetricsRequest()
            _ = continuation.yield(cachedRequest)
            logger.debug(
                "Sent cached metrics to new subscriber",
                metadata: [
                    "id": "\(id)",
                    "metricsCount": "\(latestMetrics.count)",
                ]
            )
        }

        return (id, stream)
    }

    /// Unsubscribe from metrics broadcasts.
    func unsubscribeMetrics(id: UUID) {
        if let continuation = metricsSubscribers.removeValue(forKey: id) {
            continuation.finish()
            logger.debug("Metrics subscriber removed", metadata: ["id": "\(id)"])
        }
    }

    /// Broadcast metrics to all subscribers and cache the latest values.
    func broadcastMetrics(_ request: MetricsRequest) {
        // Cache the latest metrics for new subscribers
        cacheMetrics(request)

        for (id, continuation) in metricsSubscribers {
            let result = continuation.yield(request)
            if case .terminated = result {
                metricsSubscribers.removeValue(forKey: id)
            }
        }
    }

    /// Cache metrics from a request, keeping only the latest value for each metric.
    private func cacheMetrics(_ request: MetricsRequest) {
        for resourceMetrics in request.resourceMetrics {
            let serviceName =
                resourceMetrics.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            // Cache the resource for this service
            latestMetricsResources[serviceName] = resourceMetrics.resource

            for scopeMetrics in resourceMetrics.scopeMetrics {
                for metric in scopeMetrics.metrics {
                    let key = "\(serviceName):\(metric.name)"
                    latestMetrics[key] = metric
                }
            }
        }
    }

    /// Build a MetricsRequest from cached metrics, grouped by service.
    private func buildCachedMetricsRequest() -> MetricsRequest {
        // Group metrics by service name
        var metricsByService: [String: [Opentelemetry_Proto_Metrics_V1_Metric]] = [:]

        for (key, metric) in latestMetrics {
            let serviceName = key.components(separatedBy: ":").first ?? "unknown"
            metricsByService[serviceName, default: []].append(metric)
        }

        // Build the request
        var request = MetricsRequest()
        for (serviceName, metrics) in metricsByService {
            var resourceMetrics = Opentelemetry_Proto_Metrics_V1_ResourceMetrics()

            // Use cached resource if available, otherwise create a minimal one
            if let resource = latestMetricsResources[serviceName] {
                resourceMetrics.resource = resource
            } else {
                resourceMetrics.resource = .with {
                    $0.attributes = [
                        .with {
                            $0.key = "service.name"
                            $0.value = .with { $0.stringValue = serviceName }
                        }
                    ]
                }
            }

            var scopeMetrics = Opentelemetry_Proto_Metrics_V1_ScopeMetrics()
            scopeMetrics.metrics = metrics
            resourceMetrics.scopeMetrics = [scopeMetrics]

            request.resourceMetrics.append(resourceMetrics)
        }

        return request
    }

    // MARK: - Traces

    /// Subscribe to traces broadcasts. Returns a stream that yields trace requests.
    func subscribeTraces() -> (id: UUID, stream: AsyncStream<TracesRequest>) {
        let id = UUID()
        let (stream, continuation) = AsyncStream<TracesRequest>.makeStream(
            bufferingPolicy: .bufferingNewest(100)
        )
        tracesSubscribers[id] = continuation
        logger.debug("Traces subscriber added", metadata: ["id": "\(id)"])
        return (id, stream)
    }

    /// Unsubscribe from traces broadcasts.
    func unsubscribeTraces(id: UUID) {
        if let continuation = tracesSubscribers.removeValue(forKey: id) {
            continuation.finish()
            logger.debug("Traces subscriber removed", metadata: ["id": "\(id)"])
        }
    }

    /// Broadcast traces to all subscribers.
    func broadcastTraces(_ request: TracesRequest) {
        for (id, continuation) in tracesSubscribers {
            let result = continuation.yield(request)
            if case .terminated = result {
                tracesSubscribers.removeValue(forKey: id)
            }
        }
    }
}
