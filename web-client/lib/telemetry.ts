// OTLP's int64 fields arrive as decimal strings. Keep counters as BigInt until
// after subtraction so long-running devices retain accurate CPU deltas.
export type Sample = {
  version: {
    version?: string;
    cpuArchitecture?: string;
    containerStorage?: Disk;
    partitions?: Disk[];
  };
  apps: {
    appName: string;
    runningState?: string;
    services?: { name: string; runningState?: string }[];
  }[];
  stats?: {
    host?: Host;
    containers?: {
      appName: string;
      cpuUsageNanos?: string;
      memoryBytes?: string;
    }[];
  };
  warnings?: string[];
};
type Disk = { mountpoint?: string; totalBytes?: string; usedBytes?: string };
type Host = {
  cpuTotalJiffies?: string;
  cpuIdleJiffies?: string;
  cpuCount?: number;
  memTotalBytes?: string;
  memAvailableBytes?: string;
  thermalZones?: { name?: string; tempC?: number }[];
  gpus?: {
    index?: number;
    name?: string;
    utilPercent?: number;
    memUsedBytes?: string;
    memTotalBytes?: string;
    tempC?: number;
    powerW?: number;
  }[];
  battery?: { percent?: number; state?: string };
};
export type Reading = { sample: Sample; at: number };
const counter = (v?: string) => BigInt(v || "0");
export function usage(previous: Reading | null, current: Reading) {
  const h = current.sample.stats?.host,
    p = previous?.sample.stats?.host;
  const total = counter(h?.cpuTotalJiffies) - counter(p?.cpuTotalJiffies);
  const idle = counter(h?.cpuIdleJiffies) - counter(p?.cpuIdleJiffies);
  const cpu =
    h && p && total > BigInt(0) && idle >= BigInt(0)
      ? Math.max(0, Math.min(100, 100 * (1 - Number(idle) / Number(total))))
      : null;
  const elapsed = previous ? (current.at - previous.at) * 1e6 : 0;
  const containers = new Map(
    (current.sample.stats?.containers || []).map((c) => {
      const before = previous?.sample.stats?.containers?.find(
        (p) => p.appName === c.appName,
      );
      const delta = counter(c.cpuUsageNanos) - counter(before?.cpuUsageNanos);
      return [
        c.appName,
        {
          memory: Number(c.memoryBytes || 0),
          cpu:
            before && elapsed > 0 && delta >= BigInt(0) && h?.cpuCount
              ? (Number(delta) / elapsed / h.cpuCount) * 100
              : null,
        },
      ];
    }),
  );
  return { cpu, containers };
}
export type TelemetryRow = {
  key: string;
  name: string;
  service: string;
  value: string;
  time: string;
  duration?: number;
  trace?: string;
  status?: string;
  severity?: LogSeverity;
  detail: unknown;
  metric?: MetricSample;
};
export type MetricSample = {
  time: number;
  value: number | null;
  startTime?: string;
  unit: string;
  kind: "gauge" | "sum" | "histogram" | "summary";
  temporality?: string | number;
  monotonic?: boolean;
  attributes: { [key: string]: string };
};
type AnyValue = {
  stringValue?: string;
  intValue?: string;
  doubleValue?: number;
  boolValue?: boolean;
  arrayValue?: unknown;
  kvlistValue?: unknown;
};
type Attribute = { key: string; value?: AnyValue };
type Point = {
  asDouble?: number;
  asInt?: string;
  count?: string;
  sum?: number;
  timeUnixNano?: string;
  startTimeUnixNano?: string;
  attributes?: Attribute[];
};
type Metric = {
  name: string;
  unit?: string;
  gauge?: { dataPoints?: Point[] };
  sum?: { dataPoints?: Point[]; aggregationTemporality?: string | number; isMonotonic?: boolean };
  histogram?: { dataPoints?: Point[] };
  exponentialHistogram?: { dataPoints?: Point[] };
  summary?: { dataPoints?: Point[] };
};
type Record = {
  body?: AnyValue;
  timeUnixNano?: string;
  observedTimeUnixNano?: string;
  severityText?: string;
  severityNumber?: number | string;
};
type Span = {
  name: string;
  traceId?: string;
  spanId?: string;
  startTimeUnixNano?: string;
  endTimeUnixNano?: string;
  status?: { code?: string; message?: string };
};
type Resource = {
  resource?: { attributes?: Attribute[] };
  scopeLogs?: { logRecords?: Record[] }[];
  scopeMetrics?: { scope?: { name?: string; version?: string; attributes?: Attribute[] }; metrics?: Metric[] }[];
  scopeSpans?: { spans?: Span[] }[];
};
export type OTelBatch = {
  logs?: { resourceLogs?: Resource[] };
  metrics?: { resourceMetrics?: Resource[] };
  traces?: { resourceSpans?: Resource[] };
};
function value(v?: AnyValue): string {
  return (
    v?.stringValue ??
    v?.intValue ??
    (v?.doubleValue !== undefined
      ? String(v.doubleValue)
      : v?.boolValue !== undefined
        ? String(v.boolValue)
        : v
          ? JSON.stringify(v)
          : "")
  );
}
function canonical(value: unknown): string {
  if (Array.isArray(value)) return "[" + value.map(canonical).sort().join(",") + "]";
  if (value && typeof value === "object") return "{" + Object.entries(value).sort(([a], [b]) => a.localeCompare(b)).map(([key, item]) => JSON.stringify(key) + ":" + canonical(item)).join(",") + "}";
  return JSON.stringify(value) ?? "null";
}
function time(n?: string) {
  return n && n !== "0"
    ? new Date(Number(BigInt(n) / BigInt(1000000))).toLocaleTimeString()
    : "";
}
function hex(id?: string) {
  if (!id) return "";
  try {
    return Array.from(atob(id), (c) =>
      c.charCodeAt(0).toString(16).padStart(2, "0"),
    ).join("");
  } catch {
    return id;
  }
}
type LogSeverity = "trace" | "debug" | "info" | "warn" | "error" | "fatal" | "unknown";
export function logSeverity(record: Record): LogSeverity {
  const levels: LogSeverity[] = ["trace", "debug", "info", "warn", "error", "fatal"];
  const number = Number(record.severityNumber);
  if (Number.isInteger(number) && number >= 1 && number <= 24)
    return levels[Math.floor((number - 1) / 4)];
  // OTLP protobuf JSON encodes enum values by name by default.
  const name = String(record.severityNumber || record.severityText || "")
    .replace(/^SEVERITY_NUMBER_/, "").replace(/[2-4]$/, "").toLowerCase();
  if (name === "warning") return "warn";
  if (name === "critical" || name === "panic") return "fatal";
  if (levels.includes(name as LogSeverity)) return name as LogSeverity;
  const text = record.severityText?.trim().toLowerCase();
  if (text && text !== name) return logSeverity({ severityText: text });
  return "unknown";
}
export function meetsLogLevel(level: string | undefined, minimum: string): boolean {
  if (minimum === "all") return true;
  const levels = ["trace", "debug", "info", "warn", "error", "fatal"];
  return levels.indexOf(level?.toLowerCase() || "") >= levels.indexOf(minimum.toLowerCase());
}
export function telemetryRows(kind: string, batch: OTelBatch): TelemetryRow[] {
  const resources =
    kind === "logs"
      ? batch.logs?.resourceLogs
      : kind === "metrics"
        ? batch.metrics?.resourceMetrics
        : batch.traces?.resourceSpans;
  const out: TelemetryRow[] = [];
  for (const r of resources || []) {
    const service =
      value(
        r.resource?.attributes?.find((a) => a.key === "service.name")?.value,
      ) || "Unknown service";
    if (kind === "logs")
      for (const scope of r.scopeLogs || [])
        for (const record of scope.logRecords || []) {
          const severity = logSeverity(record);
          out.push({
            key: "",
            service,
            name:
              record.severityText ||
              (severity === "unknown" ? "LOG" : severity.toUpperCase()),
            severity,
            value: value(record.body),
            time: time(record.timeUnixNano || record.observedTimeUnixNano),
            detail: { resource: r.resource, record },
          });
        }
    if (kind === "metrics")
      for (const scope of r.scopeMetrics || [])
        for (const m of scope.metrics || [])
          for (const point of (
            m.gauge ||
            m.sum ||
            m.histogram ||
            m.exponentialHistogram ||
            m.summary
          )?.dataPoints || []) {
            const labels = (point.attributes || [])
              .map((a) => `${a.key}=${value(a.value)}`)
              .sort()
              .join(", ");
            const v = point.asDouble ?? point.asInt;
            const metricKind = m.gauge ? "gauge" : m.sum ? "sum" : m.summary ? "summary" : "histogram";
            const numeric = v !== undefined ? Number(v) : Number(point.count) > 0 && point.sum !== undefined ? point.sum / Number(point.count) : null;
            out.push({
              key: JSON.stringify([service, m.name, labels, canonical(point.attributes), canonical(r.resource), canonical(scope.scope), m.unit || "", metricKind]),
              metric: {
                time: point.timeUnixNano ? Number(point.timeUnixNano) / 1e6 : NaN,
                value: numeric !== null && Number.isFinite(numeric) ? numeric : null,
                startTime: point.startTimeUnixNano,
                unit: m.unit || "",
                kind: metricKind,
                temporality: m.sum?.aggregationTemporality,
                monotonic: m.sum?.isMonotonic,
                attributes: Object.fromEntries((point.attributes || []).map((a) => [a.key, value(a.value)])),
              },
              service,
              name: m.name,
              value:
                v !== undefined
                  ? `${v}${m.unit ? " " + m.unit : ""}`
                  : `${point.count || "0"} samples${point.sum !== undefined ? ", sum " + point.sum : ""}`,
              time: time(point.timeUnixNano),
              detail: {
                resource: r.resource,
                metric: m.name,
                unit: m.unit,
                labels,
                point,
              },
            });
          }
    if (kind === "traces")
      for (const scope of r.scopeSpans || [])
        for (const span of scope.spans || []) {
          const duration =
            span.startTimeUnixNano && span.endTimeUnixNano
              ? Number(
                  BigInt(span.endTimeUnixNano) - BigInt(span.startTimeUnixNano),
                ) / 1e6
              : undefined;
          out.push({
            key: `${span.traceId}:${span.spanId}`,
            service,
            name: span.name,
            value:
              duration === undefined
                ? "Duration unavailable"
                : duration.toFixed(2) + " ms",
            time: time(span.startTimeUnixNano),
            duration,
            trace: hex(span.traceId),
            status: span.status?.code || "STATUS_CODE_UNSET",
            detail: { resource: r.resource, span },
          });
        }
  }
  return out;
}
