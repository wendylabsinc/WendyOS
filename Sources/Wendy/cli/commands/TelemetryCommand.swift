import ArgumentParser
import Dispatch
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Noora
import OpenTelemetryGRPC
import WendyAgentGRPC

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    @preconcurrency import Glibc
#elseif canImport(Musl)
    @preconcurrency import Musl
#endif

struct LogsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "logs",
        abstract: "Stream logs from your device."
    )

    @Option(name: .shortAndLong, help: "Filter logs by service name")
    var service: String?

    @Option(name: .long, help: "Filter logs by app/container name")
    var app: String?

    @Option(
        name: .shortAndLong,
        help: "Minimum severity level (trace, debug, info, warn, error, fatal)"
    )
    var level: String?

    @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
    var json: Bool = false

    @Option(
        name: .long,
        help: "Forward logs to a local OTLP collector at this address (e.g., localhost:4317)"
    )
    var forward: String?

    @OptionGroup var agentConnectionOptions: AgentConnectionOptions

    func run() async throws {
        let minSeverity: Int32? = level.flatMap { parseSeverityLevel($0) }
        // Reconnection loop - keeps trying to connect when agent restarts
        while !Task.isCancelled {
            do {
                try await withAgentGRPCClient(agentConnectionOptions, title: "") { client in
                    let telemetry = Wendy_Agent_Services_V1_WendyTelemetryService.Client(
                        wrapping: client
                    )

                    let request = Wendy_Agent_Services_V1_StreamLogsRequest.with {
                        if let service = service {
                            $0.serviceName = service
                        }
                        if let app = app {
                            $0.appName = app
                        }
                        if let minSeverity = minSeverity {
                            $0.minSeverity = minSeverity
                        }
                    }

                    let shouldForward = forward != nil

                    try await telemetry.streamLogs(request) { response in
                        switch response.accepted {
                        case .success(let contents):
                            for try await bodyPart in contents.bodyParts {
                                switch bodyPart {
                                case .message(let message):
                                    if shouldForward {
                                        // Forward to local collector (implementation TBD)
                                    }

                                    if json {
                                        printLogsAsJSON(message.logs)
                                    } else {
                                        printLogsAsText(message.logs)
                                    }
                                case .trailingMetadata:
                                    break
                                }
                            }
                        case .failure(let error):
                            throw error
                        }
                    }
                }
            } catch is CancellationError {
                throw CancellationError()
            } catch {
                if !json {
                    Noora().warning("Connection lost, reconnecting...")
                }
                try await Task.sleep(for: .seconds(2))
            }
        }
    }

    private func parseSeverityLevel(_ level: String) -> Int32? {
        switch level.lowercased() {
        case "trace": return 1
        case "debug": return 5
        case "info": return 9
        case "warn", "warning": return 13
        case "error": return 17
        case "fatal": return 21
        default: return nil
        }
    }

    private func printLogsAsText(
        _ logs: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest
    ) {
        for resourceLogs in logs.resourceLogs {
            let serviceName =
                resourceLogs.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            for scopeLogs in resourceLogs.scopeLogs {
                for record in scopeLogs.logRecords {
                    let timestamp = formatTimestamp(record.timeUnixNano)
                    let severity = formatSeverity(record.severityNumber)
                    let body = record.body.stringValue

                    print("[\(timestamp)] [\(serviceName)] [\(severity)] \(body)")
                }
            }
        }
    }

    private func printLogsAsJSON(
        _ logs: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest
    ) {
        struct LogEntry: Codable {
            let timestamp: String
            let service: String
            let severity: String
            let body: String
        }

        for resourceLogs in logs.resourceLogs {
            let serviceName =
                resourceLogs.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            for scopeLogs in resourceLogs.scopeLogs {
                for record in scopeLogs.logRecords {
                    let entry = LogEntry(
                        timestamp: formatTimestamp(record.timeUnixNano),
                        service: serviceName,
                        severity: formatSeverity(record.severityNumber),
                        body: record.body.stringValue
                    )

                    if let data = try? JSONEncoder().encode(entry),
                        let json = String(data: data, encoding: .utf8)
                    {
                        print(json)
                    }
                }
            }
        }
    }

    private func formatTimestamp(_ nanos: UInt64) -> String {
        let seconds = TimeInterval(nanos) / 1_000_000_000
        let date = Date(timeIntervalSince1970: seconds)
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.string(from: date)
    }

    private func formatSeverity(_ severity: Opentelemetry_Proto_Logs_V1_SeverityNumber) -> String {
        switch severity {
        case .trace, .trace2, .trace3, .trace4: return "TRACE"
        case .debug, .debug2, .debug3, .debug4: return "DEBUG"
        case .info, .info2, .info3, .info4: return "INFO"
        case .warn, .warn2, .warn3, .warn4: return "WARN"
        case .error, .error2, .error3, .error4: return "ERROR"
        case .fatal, .fatal2, .fatal3, .fatal4: return "FATAL"
        default: return "UNSPECIFIED"
        }
    }
}

/// Streams OpenTelemetry data as JSONL for consumption by VS Code and other tools.
struct TelemetryStreamCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "telemetry-stream",
        abstract: "Stream telemetry data as JSONL for IDE consumption."
    )

    @Flag(name: .long, help: "Include logs in the stream")
    var logs: Bool = false

    @Flag(name: .long, help: "Include metrics in the stream")
    var metrics: Bool = false

    @Option(name: .long, help: "Filter logs by app/container name")
    var app: String?

    @Option(name: .long, help: "Filter logs by service name")
    var service: String?

    @Option(
        name: .shortAndLong,
        help: "Minimum severity level for logs (trace, debug, info, warn, error, fatal)"
    )
    var level: String?

    @OptionGroup var agentConnectionOptions: AgentConnectionOptions

    func run() async throws {
        // Default to both logs and metrics if neither specified
        let streamLogs = logs || (!logs && !metrics)
        let streamMetrics = metrics || (!logs && !metrics)

        // Set up SIGINT handling for graceful shutdown
        signal(SIGINT, SIG_IGN)
        let signalSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .global())

        defer {
            signalSource.cancel()
            signal(SIGINT, SIG_DFL)
        }

        let minSeverity: Int32? = level.flatMap { parseSeverityLevel($0) }

        // Reconnection loop
        while !Task.isCancelled {
            do {
                try await withThrowingTaskGroup(of: Void.self) { group in
                    // SIGINT monitoring task
                    group.addTask {
                        await withCheckedContinuation { continuation in
                            signalSource.setEventHandler {
                                continuation.resume()
                            }
                            signalSource.resume()
                        }
                        throw CancellationError()
                    }

                    // Telemetry streaming task
                    group.addTask { [app, service] in
                        try await withAgentGRPCClient(agentConnectionOptions, title: "") { client in
                            let telemetry = Wendy_Agent_Services_V1_WendyTelemetryService.Client(
                                wrapping: client
                            )

                            try await withThrowingTaskGroup(of: Void.self) { innerGroup in
                                if streamLogs {
                                    innerGroup.addTask {
                                        let request = Wendy_Agent_Services_V1_StreamLogsRequest.with {
                                            if let service = service {
                                                $0.serviceName = service
                                            }
                                            if let app = app {
                                                $0.appName = app
                                            }
                                            if let minSeverity = minSeverity {
                                                $0.minSeverity = minSeverity
                                            }
                                        }

                                        try await telemetry.streamLogs(request) { response in
                                            switch response.accepted {
                                            case .success(let contents):
                                                for try await bodyPart in contents.bodyParts {
                                                    try Task.checkCancellation()
                                                    if case .message(let message) = bodyPart {
                                                        outputLogsAsJSONL(message.logs)
                                                    }
                                                }
                                            case .failure(let error):
                                                throw error
                                            }
                                        }
                                    }
                                }

                                if streamMetrics {
                                    innerGroup.addTask {
                                        let request = Wendy_Agent_Services_V1_StreamMetricsRequest()

                                        try await telemetry.streamMetrics(request) { response in
                                            switch response.accepted {
                                            case .success(let contents):
                                                for try await bodyPart in contents.bodyParts {
                                                    try Task.checkCancellation()
                                                    if case .message(let message) = bodyPart {
                                                        outputMetricsAsJSONL(message.metrics)
                                                    }
                                                }
                                            case .failure(let error):
                                                throw error
                                            }
                                        }
                                    }
                                }

                                try await innerGroup.waitForAll()
                            }
                        }
                    }

                    try await group.next()
                    group.cancelAll()
                }
            } catch is CancellationError {
                break
            } catch {
                // Output connection error as JSONL
                outputErrorAsJSONL("Connection lost, reconnecting...")
                try await Task.sleep(for: .seconds(2))
            }
        }
    }

    private func parseSeverityLevel(_ level: String) -> Int32? {
        switch level.lowercased() {
        case "trace": return 1
        case "debug": return 5
        case "info": return 9
        case "warn", "warning": return 13
        case "error": return 17
        case "fatal": return 21
        default: return nil
        }
    }

    private func outputLogsAsJSONL(
        _ logs: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest
    ) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]

        for resourceLogs in logs.resourceLogs {
            let serviceName =
                resourceLogs.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            // Extract resource attributes
            var resourceAttrs: [String: String] = [:]
            for attr in resourceLogs.resource.attributes {
                resourceAttrs[attr.key] = attr.value.stringValue
            }

            for scopeLogs in resourceLogs.scopeLogs {
                for record in scopeLogs.logRecords {
                    // Extract log record attributes
                    var logAttrs: [String: String] = [:]
                    for attr in record.attributes {
                        logAttrs[attr.key] = attr.value.stringValue
                    }

                    let entry = LogJSONLEntry(
                        type: "log",
                        timestamp: formatTimestamp(record.timeUnixNano),
                        timestampNano: record.timeUnixNano,
                        service: serviceName,
                        severity: formatSeverity(record.severityNumber),
                        severityNumber: Int(record.severityNumber.rawValue),
                        body: record.body.stringValue,
                        attributes: logAttrs,
                        resource: resourceAttrs
                    )

                    if let data = try? encoder.encode(entry),
                        let json = String(data: data, encoding: .utf8)
                    {
                        print(json)
                        fflush(stdout)
                    }
                }
            }
        }
    }

    private func outputMetricsAsJSONL(
        _ metrics: Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest
    ) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]

        for resourceMetrics in metrics.resourceMetrics {
            let serviceName =
                resourceMetrics.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            // Extract resource attributes
            var resourceAttrs: [String: String] = [:]
            for attr in resourceMetrics.resource.attributes {
                resourceAttrs[attr.key] = attr.value.stringValue
            }

            for scopeMetrics in resourceMetrics.scopeMetrics {
                for metric in scopeMetrics.metrics {
                    let (value, metricType) = extractMetricValue(metric)

                    let entry = MetricJSONLEntry(
                        type: "metric",
                        timestamp: formatCurrentTimestamp(),
                        service: serviceName,
                        name: metric.name,
                        description: metric.description_p,
                        unit: metric.unit,
                        metricType: metricType,
                        value: value,
                        resource: resourceAttrs
                    )

                    if let data = try? encoder.encode(entry),
                        let json = String(data: data, encoding: .utf8)
                    {
                        print(json)
                        fflush(stdout)
                    }
                }
            }
        }
    }

    private func outputErrorAsJSONL(_ message: String) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]

        let entry = ErrorJSONLEntry(
            type: "error",
            timestamp: formatCurrentTimestamp(),
            message: message
        )

        if let data = try? encoder.encode(entry),
            let json = String(data: data, encoding: .utf8)
        {
            print(json)
            fflush(stdout)
        }
    }

    private func extractMetricValue(_ metric: Opentelemetry_Proto_Metrics_V1_Metric) -> (
        Double?, String
    ) {
        switch metric.data {
        case .gauge(let gauge):
            if let point = gauge.dataPoints.last {
                switch point.value {
                case .asDouble(let d): return (d, "gauge")
                case .asInt(let i): return (Double(i), "gauge")
                default: return (nil, "gauge")
                }
            }
            return (nil, "gauge")
        case .sum(let sum):
            if let point = sum.dataPoints.last {
                switch point.value {
                case .asDouble(let d): return (d, "sum")
                case .asInt(let i): return (Double(i), "sum")
                default: return (nil, "sum")
                }
            }
            return (nil, "sum")
        case .histogram(let histogram):
            if let point = histogram.dataPoints.last {
                return (point.sum / Double(max(point.count, 1)), "histogram")
            }
            return (nil, "histogram")
        case .summary(let summary):
            if let point = summary.dataPoints.last {
                return (point.sum / Double(max(point.count, 1)), "summary")
            }
            return (nil, "summary")
        default:
            return (nil, "unknown")
        }
    }

    private func formatTimestamp(_ nanos: UInt64) -> String {
        let seconds = TimeInterval(nanos) / 1_000_000_000
        let date = Date(timeIntervalSince1970: seconds)
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.string(from: date)
    }

    private func formatCurrentTimestamp() -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.string(from: Date())
    }

    private func formatSeverity(_ severity: Opentelemetry_Proto_Logs_V1_SeverityNumber) -> String {
        switch severity {
        case .trace, .trace2, .trace3, .trace4: return "TRACE"
        case .debug, .debug2, .debug3, .debug4: return "DEBUG"
        case .info, .info2, .info3, .info4: return "INFO"
        case .warn, .warn2, .warn3, .warn4: return "WARN"
        case .error, .error2, .error3, .error4: return "ERROR"
        case .fatal, .fatal2, .fatal3, .fatal4: return "FATAL"
        default: return "UNSPECIFIED"
        }
    }
}

// MARK: - JSONL Entry Types

private struct LogJSONLEntry: Encodable {
    let type: String
    let timestamp: String
    let timestampNano: UInt64
    let service: String
    let severity: String
    let severityNumber: Int
    let body: String
    let attributes: [String: String]
    let resource: [String: String]
}

private struct MetricJSONLEntry: Encodable {
    let type: String
    let timestamp: String
    let service: String
    let name: String
    let description: String
    let unit: String
    let metricType: String
    let value: Double?
    let resource: [String: String]
}

private struct ErrorJSONLEntry: Encodable {
    let type: String
    let timestamp: String
    let message: String
}
