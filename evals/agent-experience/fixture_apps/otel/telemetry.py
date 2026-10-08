"""Export real OTLP protobufs to the local Wendy collector."""

import time
import urllib.request
import uuid

from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
from opentelemetry.proto.resource.v1.resource_pb2 import Resource
from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import ExportLogsServiceRequest
from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import ExportMetricsServiceRequest
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from opentelemetry.proto.logs.v1.logs_pb2 import ResourceLogs, ScopeLogs, LogRecord
from opentelemetry.proto.metrics.v1.metrics_pb2 import ResourceMetrics, ScopeMetrics, Metric, Gauge, NumberDataPoint
from opentelemetry.proto.trace.v1.trace_pb2 import ResourceSpans, ScopeSpans, Span, Status


def attrs(values):
    return [KeyValue(key=k, value=AnyValue(string_value=str(v))) for k, v in values.items()]


def emit(settings, challenge, started, ended, error):
    resource = Resource(attributes=attrs({"service.name": settings["app_id"],
        "wendy.app.name": settings["app_id"], "eval.run_id": settings["nonce"]}))
    attributes = attrs({"eval.challenge": challenge, "eval.run_id": settings["nonce"]})
    log = LogRecord(time_unix_nano=ended, severity_number=17 if error else 9,
                    body=AnyValue(string_value="eval.worker: " + (error or "request complete")), attributes=attributes)
    metric = Metric(name="eval.processing_ms", unit="ms", gauge=Gauge(data_points=[NumberDataPoint(
        time_unix_nano=ended, as_double=(ended-started)/1e6, attributes=attributes)]))
    trace, parent, child = uuid.uuid4().bytes, uuid.uuid4().bytes[:8], uuid.uuid4().bytes[:8]
    common = dict(trace_id=trace, start_time_unix_nano=started, end_time_unix_nano=ended,
                  attributes=attributes, status=Status(code=2 if error else 1, message=error))
    spans = [Span(name="eval.request", span_id=parent, kind=2, **common),
             Span(name="eval.worker", span_id=child, parent_span_id=parent, kind=1, **common)]
    records = {
        "logs": ExportLogsServiceRequest(resource_logs=[ResourceLogs(resource=resource, scope_logs=[ScopeLogs(log_records=[log])])]),
        "metrics": ExportMetricsServiceRequest(resource_metrics=[ResourceMetrics(resource=resource, scope_metrics=[ScopeMetrics(metrics=[metric])])]),
        "traces": ExportTraceServiceRequest(resource_spans=[ResourceSpans(resource=resource, scope_spans=[ScopeSpans(spans=spans)])]),
    }
    for signal, record in records.items():
        request = urllib.request.Request("http://127.0.0.1:4318/v1/" + signal, record.SerializeToString(),
                                         {"Content-Type": "application/x-protobuf"})
        with urllib.request.urlopen(request, timeout=3) as response:
            response.read()
