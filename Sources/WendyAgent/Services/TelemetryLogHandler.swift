import Foundation
import Logging
import OpenTelemetryGRPC

/// Holds a reference to the TelemetryBroadcaster that can be set after logging is bootstrapped.
final class TelemetryLogBroadcasterHolder: @unchecked Sendable {
    static let shared = TelemetryLogBroadcasterHolder()

    private var _broadcaster: TelemetryBroadcaster?
    private let lock = NSLock()

    var broadcaster: TelemetryBroadcaster? {
        get {
            lock.lock()
            defer { lock.unlock() }
            return _broadcaster
        }
        set {
            lock.lock()
            defer { lock.unlock() }
            _broadcaster = newValue
        }
    }
}

/// A LogHandler that broadcasts log messages to CLI clients via the TelemetryBroadcaster.
struct TelemetryLogHandler: LogHandler {
    let label: String
    var metadata: Logger.Metadata = [:]
    var logLevel: Logger.Level = .info

    subscript(metadataKey key: String) -> Logger.Metadata.Value? {
        get { metadata[key] }
        set { metadata[key] = newValue }
    }

    func log(
        level: Logger.Level,
        message: Logger.Message,
        metadata: Logger.Metadata?,
        source: String,
        file: String,
        function: String,
        line: UInt
    ) {
        guard let broadcaster = TelemetryLogBroadcasterHolder.shared.broadcaster else {
            return
        }

        let mergedMetadata = self.metadata.merging(metadata ?? [:]) { _, new in new }

        // Create OTel log record
        let logRequest = createLogRequest(
            level: level,
            message: message.description,
            metadata: mergedMetadata,
            source: source,
            label: label
        )

        // Broadcast asynchronously
        Task {
            await broadcaster.broadcastLogs(logRequest)
        }
    }

    private func createLogRequest(
        level: Logger.Level,
        message: String,
        metadata: Logger.Metadata,
        source: String,
        label: String
    ) -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest {
        let timestamp = UInt64(Date().timeIntervalSince1970 * 1_000_000_000)

        var logRecord = Opentelemetry_Proto_Logs_V1_LogRecord()
        logRecord.timeUnixNano = timestamp
        logRecord.observedTimeUnixNano = timestamp
        logRecord.severityNumber = otelSeverity(from: level)
        logRecord.severityText = level.rawValue.uppercased()
        logRecord.body = .with { $0.stringValue = message }

        // Add metadata as attributes
        for (key, value) in metadata {
            var attr = Opentelemetry_Proto_Common_V1_KeyValue()
            attr.key = key
            attr.value = .with { $0.stringValue = "\(value)" }
            logRecord.attributes.append(attr)
        }

        // Add source as attribute
        var sourceAttr = Opentelemetry_Proto_Common_V1_KeyValue()
        sourceAttr.key = "code.namespace"
        sourceAttr.value = .with { $0.stringValue = source }
        logRecord.attributes.append(sourceAttr)

        var scopeLogs = Opentelemetry_Proto_Logs_V1_ScopeLogs()
        scopeLogs.logRecords = [logRecord]

        var resourceLogs = Opentelemetry_Proto_Logs_V1_ResourceLogs()
        resourceLogs.scopeLogs = [scopeLogs]

        // Add service name attribute
        var serviceNameAttr = Opentelemetry_Proto_Common_V1_KeyValue()
        serviceNameAttr.key = "service.name"
        serviceNameAttr.value = .with { $0.stringValue = "wendy-agent" }
        resourceLogs.resource.attributes.append(serviceNameAttr)

        // Add logger label as wendy.app.name for filtering
        var appNameAttr = Opentelemetry_Proto_Common_V1_KeyValue()
        appNameAttr.key = "wendy.app.name"
        appNameAttr.value = .with { $0.stringValue = label }
        resourceLogs.resource.attributes.append(appNameAttr)

        return Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest.with {
            $0.resourceLogs = [resourceLogs]
        }
    }

    private func otelSeverity(
        from level: Logger.Level
    ) -> Opentelemetry_Proto_Logs_V1_SeverityNumber {
        switch level {
        case .trace: return .trace
        case .debug: return .debug
        case .info: return .info
        case .notice: return .info2
        case .warning: return .warn
        case .error: return .error
        case .critical: return .fatal
        }
    }
}

/// A multiplex log handler that sends logs to multiple handlers.
struct MultiplexLogHandler: LogHandler {
    private var handlers: [LogHandler]

    var metadata: Logger.Metadata {
        get { handlers.first?.metadata ?? [:] }
        set {
            for i in handlers.indices {
                handlers[i].metadata = newValue
            }
        }
    }

    var logLevel: Logger.Level {
        get { handlers.first?.logLevel ?? .info }
        set {
            for i in handlers.indices {
                handlers[i].logLevel = newValue
            }
        }
    }

    init(_ handlers: [LogHandler]) {
        self.handlers = handlers
    }

    subscript(metadataKey key: String) -> Logger.Metadata.Value? {
        get { handlers.first?[metadataKey: key] }
        set {
            for i in handlers.indices {
                handlers[i][metadataKey: key] = newValue
            }
        }
    }

    func log(
        level: Logger.Level,
        message: Logger.Message,
        metadata: Logger.Metadata?,
        source: String,
        file: String,
        function: String,
        line: UInt
    ) {
        for handler in handlers {
            handler.log(
                level: level,
                message: message,
                metadata: metadata,
                source: source,
                file: file,
                function: function,
                line: line
            )
        }
    }
}
