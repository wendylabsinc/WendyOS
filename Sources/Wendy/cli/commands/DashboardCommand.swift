import ArgumentParser
import Foundation
import GRPCCore
import Noora
import OpenTelemetryGRPC
import WendyAgentGRPC

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    @preconcurrency import Glibc
#elseif canImport(Musl)
    @preconcurrency import Musl
#elseif os(Windows)
    import WinSDK
#endif

/// Flush stdout - fflush is thread-safe and we're doing synchronous terminal I/O.
/// On Linux, we use fflush(nil) to avoid Swift 6 concurrency warnings about the
/// stdout global variable. This flushes all output streams, which is safe but
/// slightly less efficient. On other platforms we can use stdout directly.
@inline(__always)
private func flushStdout() {
    #if os(Linux)
        fflush(nil)
    #else
        fflush(stdout)
    #endif
}

struct DashboardCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "dashboard",
        abstract: "Live dashboard showing device metrics and logs."
    )

    @Option(name: .shortAndLong, help: "Refresh interval in seconds")
    var interval: Double = 1.0

    @Option(name: .long, help: "Filter logs by service name")
    var service: String?

    @Option(name: .long, help: "Filter logs by app/container name")
    var app: String?

    @OptionGroup var agentConnectionOptions: AgentConnectionOptions

    func run() async throws {
        let endpoint = try await agentConnectionOptions.read(
            title: "For which device do you want to view the dashboard?"
        )

        let dashboard = Dashboard()

        // Enter alternate screen buffer and hide cursor
        print("\u{001B}[?1049h", terminator: "")  // Enter alternate screen
        print("\u{001B}[?25l", terminator: "")  // Hide cursor
        flushStdout()

        // Ensure we restore terminal state on exit
        defer {
            print("\u{001B}[?25h", terminator: "")  // Show cursor
            print("\u{001B}[?1049l", terminator: "")  // Exit alternate screen
            flushStdout()
        }

        try await withThrowingTaskGroup(of: Void.self) { group in
            // Metrics streaming task
            group.addTask {
                while !Task.isCancelled {
                    do {
                        try await withAgentGRPCClient(endpoint, title: "") { client in
                            let telemetry = Wendy_Agent_Services_V1_WendyTelemetryService.Client(
                                wrapping: client
                            )

                            let request = Wendy_Agent_Services_V1_StreamMetricsRequest()

                            try await telemetry.streamMetrics(request) { response in
                                switch response.accepted {
                                case .success(let contents):
                                    for try await bodyPart in contents.bodyParts {
                                        switch bodyPart {
                                        case .message(let message):
                                            await dashboard.updateMetrics(with: message.metrics)
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
                        break
                    } catch {
                        await dashboard.setMetricsConnectionStatus(connected: false)
                        try await Task.sleep(for: .seconds(2))
                    }
                }
            }

            // Logs streaming task
            group.addTask { [service, app] in
                while !Task.isCancelled {
                    do {
                        try await withAgentGRPCClient(endpoint, title: "") { client in
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
                            }

                            try await telemetry.streamLogs(request) { response in
                                switch response.accepted {
                                case .success(let contents):
                                    for try await bodyPart in contents.bodyParts {
                                        switch bodyPart {
                                        case .message(let message):
                                            await dashboard.updateLogs(with: message.logs)
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
                        break
                    } catch {
                        await dashboard.setLogsConnectionStatus(connected: false)
                        try await Task.sleep(for: .seconds(2))
                    }
                }
            }

            // Render loop task
            group.addTask { [interval] in
                await dashboard.setMetricsConnectionStatus(connected: true)
                await dashboard.setLogsConnectionStatus(connected: true)
                while !Task.isCancelled {
                    await dashboard.render()
                    try await Task.sleep(for: .seconds(interval))
                }
            }

            // Wait for any task to complete (cancellation)
            try await group.next()
            group.cancelAll()
        }
    }
}

/// Get the current terminal height in rows
private func getTerminalHeight() -> Int {
    #if os(Windows)
        var csbi = CONSOLE_SCREEN_BUFFER_INFO()
        if GetConsoleScreenBufferInfo(GetStdHandle(STD_OUTPUT_HANDLE), &csbi) {
            return Int(csbi.srWindow.Bottom - csbi.srWindow.Top + 1)
        }
        // Random Default fallback
        return 24
    #else
        var ws = winsize()
        if ioctl(STDOUT_FILENO, UInt(TIOCGWINSZ), &ws) == 0 && ws.ws_row > 0 {
            return Int(ws.ws_row)
        }
        // Fallback: check LINES environment variable
        if let linesStr = ProcessInfo.processInfo.environment["LINES"],
            let lines = Int(linesStr)
        {
            return lines
        }
        // Default fallback
        return 24
    #endif
}

actor Dashboard {
    private var metrics: [String: MetricData] = [:]
    private var logs: [LogEntry] = []
    private var lastMetricsUpdate: Date?
    private var lastLogsUpdate: Date?
    private var metricsConnected = false
    private var logsConnected = false

    struct MetricData {
        let name: String
        let service: String
        var value: String
        var unit: String
        var timestamp: Date
    }

    struct LogEntry {
        let timestamp: Date
        let service: String
        let severity: String
        let body: String
    }

    /// Calculate how many log lines we can display based on terminal height
    /// Reserves space for: header (3), status (2), metrics section header (1),
    /// metrics content (estimated), logs section header (1), footer (1)
    private func calculateMaxLogs() -> Int {
        let terminalHeight = getTerminalHeight()
        // Count metrics lines: each service has 1 header + N metrics + 1 blank
        let metricsLines: Int
        if metrics.isEmpty {
            metricsLines = 1  // "Waiting for metrics..."
        } else {
            let grouped = Dictionary(grouping: metrics.values) { $0.service }
            metricsLines = grouped.reduce(0) { total, entry in
                total + 1 + entry.value.count + 1  // header + metrics + blank
            }
        }
        // Fixed overhead: header (3) + status (2) + blank (1) + metrics header (1) + logs header (1) + footer (2)
        let fixedOverhead = 10
        let availableForLogs = terminalHeight - fixedOverhead - metricsLines
        return max(5, availableForLogs)  // At least 5 log lines
    }

    func setMetricsConnectionStatus(connected: Bool) {
        metricsConnected = connected
    }

    func setLogsConnectionStatus(connected: Bool) {
        logsConnected = connected
    }

    func updateMetrics(
        with metricsRequest: Opentelemetry_Proto_Collector_Metrics_V1_ExportMetricsServiceRequest
    ) {
        lastMetricsUpdate = Date()
        metricsConnected = true

        for resourceMetrics in metricsRequest.resourceMetrics {
            let serviceName =
                resourceMetrics.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            for scopeMetrics in resourceMetrics.scopeMetrics {
                for metric in scopeMetrics.metrics {
                    let key = "\(serviceName):\(metric.name)"
                    metrics[key] = MetricData(
                        name: metric.name,
                        service: serviceName,
                        value: formatValue(metric),
                        unit: metric.unit,
                        timestamp: Date()
                    )
                }
            }
        }
    }

    func updateLogs(
        with logsRequest: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest
    ) {
        lastLogsUpdate = Date()
        logsConnected = true

        for resourceLogs in logsRequest.resourceLogs {
            let serviceName =
                resourceLogs.resource.attributes
                .first { $0.key == "service.name" }?.value.stringValue ?? "unknown"

            for scopeLogs in resourceLogs.scopeLogs {
                for record in scopeLogs.logRecords {
                    let seconds = TimeInterval(record.timeUnixNano) / 1_000_000_000
                    let entry = LogEntry(
                        timestamp: Date(timeIntervalSince1970: seconds),
                        service: serviceName,
                        severity: formatSeverity(record.severityNumber),
                        body: record.body.stringValue
                    )
                    logs.append(entry)
                }
            }
        }

        // Keep a reasonable buffer of logs (trim when too large)
        let maxBuffer = 100
        if logs.count > maxBuffer {
            logs = Array(logs.suffix(maxBuffer))
        }
    }

    func render() {
        // Move cursor to home position (top-left)
        print("\u{001B}[H", terminator: "")

        let resetColor = "\u{001B}[0m"
        let bold = "\u{001B}[1m"
        let dim = "\u{001B}[2m"
        let clearLine = "\u{001B}[K"  // Clear from cursor to end of line

        // Header
        print(
            "╔════════════════════════════════════════════════════════════════════════════╗\(clearLine)"
        )
        print(
            "║                         WENDY DEVICE DASHBOARD                             ║\(clearLine)"
        )
        print(
            "╚════════════════════════════════════════════════════════════════════════════╝\(clearLine)"
        )

        // Connection status
        let metricsStatus =
            metricsConnected
            ? "\u{001B}[32m●\(resetColor) Metrics" : "\u{001B}[31m○\(resetColor) Metrics"
        let logsStatus =
            logsConnected ? "\u{001B}[32m●\(resetColor) Logs" : "\u{001B}[31m○\(resetColor) Logs"
        print("\(metricsStatus)  \(logsStatus)\(clearLine)")
        print(clearLine)

        // ═══════════════════════════════════════════════════════════════════════════
        // METRICS SECTION
        // ═══════════════════════════════════════════════════════════════════════════
        print("\(bold)━━━ METRICS ━━━\(resetColor)\(clearLine)")

        if metrics.isEmpty {
            print("\(dim)  Waiting for metrics...\(resetColor)\(clearLine)")
        } else {
            // Group metrics by service
            let grouped = Dictionary(grouping: metrics.values) { $0.service }

            for (service, serviceMetrics) in grouped.sorted(by: { $0.key < $1.key }) {
                print("┌─ \(bold)\(service)\(resetColor)\(clearLine)")

                let sortedMetrics = serviceMetrics.sorted { $0.name < $1.name }
                for (index, metric) in sortedMetrics.enumerated() {
                    let prefix = index == sortedMetrics.count - 1 ? "└" : "├"
                    let unitStr = metric.unit.isEmpty ? "" : " \(metric.unit)"
                    let valueStr = formatDisplayValue(metric.value, unit: metric.unit)
                    print("\(prefix)── \(metric.name): \(valueStr)\(unitStr)\(clearLine)")
                }
            }
        }
        print(clearLine)

        // ═══════════════════════════════════════════════════════════════════════════
        // LOGS SECTION
        // ═══════════════════════════════════════════════════════════════════════════
        print("\(bold)━━━ LOGS ━━━\(resetColor)\(clearLine)")

        if logs.isEmpty {
            print("\(dim)  Waiting for logs...\(resetColor)\(clearLine)")
        } else {
            let formatter = DateFormatter()
            formatter.dateFormat = "HH:mm:ss"

            let maxLogs = calculateMaxLogs()
            let recentLogs = logs.suffix(maxLogs)
            for entry in recentLogs {
                let time = formatter.string(from: entry.timestamp)
                let severityColor = colorForSeverity(entry.severity)
                let truncatedBody =
                    entry.body.count > 60 ? String(entry.body.prefix(60)) + "..." : entry.body
                print(
                    "\(dim)\(time)\(resetColor) \(severityColor)\(entry.severity.padding(toLength: 5, withPad: " ", startingAt: 0))\(resetColor) [\(entry.service)] \(truncatedBody)\(clearLine)"
                )
            }
        }
        print(clearLine)

        print("\(dim)Press Ctrl+C to exit\(resetColor)\(clearLine)")

        // Clear any remaining lines below (in case content shrunk)
        print("\u{001B}[J", terminator: "")

        // Flush output to ensure immediate display
        flushStdout()
    }

    private func formatValue(_ metric: Opentelemetry_Proto_Metrics_V1_Metric) -> String {
        switch metric.data {
        case .gauge(let gauge):
            if let point = gauge.dataPoints.last {
                switch point.value {
                case .asDouble(let d): return String(format: "%.2f", d)
                case .asInt(let i): return String(i)
                default: return "N/A"
                }
            }
        case .sum(let sum):
            if let point = sum.dataPoints.last {
                switch point.value {
                case .asDouble(let d): return String(format: "%.2f", d)
                case .asInt(let i): return String(i)
                default: return "N/A"
                }
            }
        case .histogram(let histogram):
            if let point = histogram.dataPoints.last {
                return String(format: "%.2f", point.sum / Double(max(point.count, 1)))
            }
        case .summary(let summary):
            if let point = summary.dataPoints.last {
                return String(format: "%.2f", point.sum / Double(max(point.count, 1)))
            }
        default:
            break
        }
        return "N/A"
    }

    private func formatDisplayValue(_ value: String, unit: String) -> String {
        guard let doubleValue = Double(value) else { return value }

        // Format bytes
        if unit == "By" || unit == "bytes" {
            if doubleValue >= 1_073_741_824 {
                return String(format: "%.1f GB", doubleValue / 1_073_741_824)
            } else if doubleValue >= 1_048_576 {
                return String(format: "%.1f MB", doubleValue / 1_048_576)
            } else if doubleValue >= 1024 {
                return String(format: "%.1f KB", doubleValue / 1024)
            }
            return String(format: "%.0f B", doubleValue)
        }

        // Format percentages
        if unit == "%" || unit == "1" {
            return String(format: "%.1f%%", doubleValue * (unit == "1" ? 100 : 1))
        }

        // Format durations
        if unit == "s" || unit == "seconds" {
            if doubleValue < 0.001 {
                return String(format: "%.2f µs", doubleValue * 1_000_000)
            } else if doubleValue < 1 {
                return String(format: "%.2f ms", doubleValue * 1000)
            }
            return String(format: "%.2f s", doubleValue)
        }

        return value
    }

    private func formatSeverity(_ severity: Opentelemetry_Proto_Logs_V1_SeverityNumber) -> String {
        switch severity {
        case .trace, .trace2, .trace3, .trace4: return "TRACE"
        case .debug, .debug2, .debug3, .debug4: return "DEBUG"
        case .info, .info2, .info3, .info4: return "INFO"
        case .warn, .warn2, .warn3, .warn4: return "WARN"
        case .error, .error2, .error3, .error4: return "ERROR"
        case .fatal, .fatal2, .fatal3, .fatal4: return "FATAL"
        default: return "UNSP"
        }
    }

    private func colorForSeverity(_ severity: String) -> String {
        switch severity {
        case "TRACE": return "\u{001B}[90m"  // Gray
        case "DEBUG": return "\u{001B}[36m"  // Cyan
        case "INFO": return "\u{001B}[32m"  // Green
        case "WARN": return "\u{001B}[33m"  // Yellow
        case "ERROR": return "\u{001B}[31m"  // Red
        case "FATAL": return "\u{001B}[35m"  // Magenta
        default: return "\u{001B}[0m"  // Reset
        }
    }
}
