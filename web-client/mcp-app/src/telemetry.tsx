type RecordValue = Record<string, unknown>;
type MetricPoint = { time: number; value: number };
export type MetricSeries = {
  id: string;
  name: string;
  unit: string;
  labels: string;
  meaning: string;
  latest: RecordValue;
  latestValue: unknown;
  points: MetricPoint[];
};

function record(value: unknown): RecordValue {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as RecordValue)
    : {};
}
function text(value: unknown): string {
  if (value == null) return "";
  if (typeof value === "object")
    return Object.entries(value)
      .map(([k, v]) => `${k}: ${text(v)}`)
      .join(" · ");
  return String(value);
}

export function telemetryTime(row: RecordValue): number | undefined {
  for (const field of ["timeUnixNano", "observedTimeUnixNano"]) {
    const raw = row[field];
    if (raw == null || raw === "0" || raw === 0) continue;
    // OTLP timestamps are decimal strings, not JavaScript-safe integers.
    try {
      const time = Number(BigInt(String(raw)) / 1_000_000n);
      if (time > 0 && Number.isFinite(new Date(time).valueOf())) return time;
    } catch {
      /* Try the next explicitly timestamped field. */
    }
  }
  for (const field of ["observed_at", "timestamp"]) {
    const raw = row[field];
    if (typeof raw !== "string" || !raw.includes("T")) continue;
    const time = Date.parse(raw);
    if (Number.isFinite(time)) return time;
  }
}

function clock(time: number) {
  return new Date(time).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}
function timestamp(row: RecordValue) {
  const time = telemetryTime(row);
  return time == null ? "Time unavailable" : clock(time);
}
function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(",")}]`;
  if (value && typeof value === "object")
    return `{${Object.entries(value)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${JSON.stringify(k)}:${stable(v)}`)
      .join(",")}}`;
  return JSON.stringify(value) ?? "null";
}
function numeric(value: unknown): number | undefined {
  if (typeof value !== "number" && typeof value !== "string") return;
  if (typeof value === "string" && !value.trim()) return;
  const number = Number(value);
  // Preserve large integer text in the value label; don't graph rounded values.
  if (!Number.isFinite(number)) return;
  if (Number.isInteger(number) && !Number.isSafeInteger(number)) return;
  return number;
}
function measurement(row: RecordValue) {
  const distribution = [
    "histogram",
    "exponentialHistogram",
    "summary",
  ].includes(text(row.kind));
  if (distribution && row.sum != null)
    return { value: row.sum, field: "sum", label: "Sum of observations" };
  if (distribution && row.count != null)
    return { value: row.count, field: "count", label: "Observation count" };
  return {
    value: row.value ?? row.asDouble ?? row.asInt,
    field: "value",
    label: "",
  };
}

// Only join points from the same OTLP series. Different services, CPU modes,
// units, and aggregation meanings must never become a synthetic time series.
export function metricSeries(rows: RecordValue[]): MetricSeries[] {
  const groups = new Map<string, MetricSeries>();
  for (const row of rows) {
    const value = measurement(row);
    const unit = value.field === "count" ? "observations" : text(row.unit);
    const id = stable([
      row.name,
      unit,
      row.kind,
      row.aggregationTemporality,
      row.isMonotonic,
      row.resource,
      row.scope,
      row.attributes,
      value.field,
    ]);
    let series = groups.get(id);
    if (!series) {
      const resource = record(row.resource);
      const source = text(
        resource["service.name"] || resource["container.name"],
      );
      const labels = text(row.attributes);
      const cumulative = text(row.aggregationTemporality).includes(
        "CUMULATIVE",
      );
      const delta = text(row.aggregationTemporality).includes("DELTA");
      series = {
        id,
        name: text(row.name) || "Unnamed metric",
        unit,
        labels: [source, labels].filter(Boolean).join(" · "),
        meaning: [
          value.label,
          cumulative ? "Cumulative total" : delta ? "Per interval" : "",
        ]
          .filter(Boolean)
          .join(" · "),
        latest: row,
        latestValue: value.value,
        points: [],
      };
      groups.set(id, series);
    }
    const time = telemetryTime(row);
    const latestTime = telemetryTime(series.latest);
    if (latestTime == null || (time != null && time >= latestTime)) {
      series.latest = row;
      series.latestValue = value.value;
    }
    const number = numeric(value.value);
    // OTLP flags=1 marks a missing recorded value, including a default zero.
    if (time != null && number != null && (Number(row.flags) & 1) === 0)
      series.points.push({ time, value: number });
  }
  for (const series of groups.values()) {
    // History replay may include the cached current point again.
    series.points = Array.from(
      new Map(series.points.map((point) => [point.time, point])).values(),
    ).sort((a, b) => a.time - b.time);
  }
  return Array.from(groups.values());
}

export function metricValue(value: unknown, unit: string): string {
  const number = numeric(value);
  const suffix = unit === "1" ? "" : unit;
  if (number == null)
    return [text(value) || "Unavailable", suffix].filter(Boolean).join(" ");
  if (["By", "B", "bytes"].includes(unit)) {
    const index = Math.min(
      4,
      Math.max(0, Math.floor(Math.log2(Math.abs(number) || 1) / 10)),
    );
    return `${(number / 1024 ** index).toLocaleString(undefined, { maximumFractionDigits: 2 })} ${["B", "KiB", "MiB", "GiB", "TiB"][index]}`;
  }
  const formatted = number.toLocaleString(undefined, {
    maximumFractionDigits: 3,
    ...(number !== 0 && Math.abs(number) < 0.001
      ? { notation: "scientific" as const }
      : {}),
  });
  return [formatted, suffix].filter(Boolean).join(" ");
}

function MetricChart({ series }: { series: MetricSeries }) {
  const points = series.points;
  if (points.length < 2)
    return (
      <div className="metric-no-history">
        <span className="metric-no-history-mark" aria-hidden="true">
          ·
        </span>
        <span>
          {points.length === 1
            ? "One timestamped sample. More samples are needed to show a trend."
            : "No timestamped numeric samples to chart."}
        </span>
      </div>
    );
  const first = points[0],
    last = points[points.length - 1];
  const min = Math.min(...points.map((p) => p.value));
  const max = Math.max(...points.map((p) => p.value));
  const padding = (max - min || Math.abs(max) || 1) * 0.12;
  const low = min - padding,
    high = max + padding;
  const x = (point: MetricPoint) =>
    12 + ((point.time - first.time) / (last.time - first.time)) * 336;
  const y = (point: MetricPoint) =>
    118 - ((point.value - low) / (high - low)) * 104;
  const path = points
    .map((p, i) => `${i ? "L" : "M"}${x(p).toFixed(2)},${y(p).toFixed(2)}`)
    .join(" ");
  const description = `${series.name}: ${points.length} samples from ${new Date(first.time).toLocaleString()} to ${new Date(last.time).toLocaleString()}. Low ${metricValue(min, series.unit)}, high ${metricValue(max, series.unit)}.`;
  return (
    <div className="metric-chart">
      <div className="metric-range">
        <span>High {metricValue(max, series.unit)}</span>
        <span>Low {metricValue(min, series.unit)}</span>
      </div>
      <svg viewBox="0 0 360 132" role="img" aria-label={description}>
        <title>{description}</title>
        {[14, 66, 118].map((line) => (
          <line
            key={line}
            x1="12"
            x2="348"
            y1={line}
            y2={line}
            className="metric-gridline"
          />
        ))}
        <path d={path} className="metric-line" />
        {points.map((p) => (
          <circle
            key={p.time}
            cx={x(p)}
            cy={y(p)}
            r={points.length < 20 ? 3 : 2}
            className="metric-point"
          >
            <title>{`${new Date(p.time).toLocaleString()} · ${metricValue(p.value, series.unit)}`}</title>
          </circle>
        ))}
      </svg>
      <div className="metric-time-range">
        <time
          dateTime={new Date(first.time).toISOString()}
          title={new Date(first.time).toLocaleString()}
        >
          {clock(first.time)}
        </time>
        <time
          dateTime={new Date(last.time).toISOString()}
          title={new Date(last.time).toLocaleString()}
        >
          {clock(last.time)}
        </time>
      </div>
      <small className="metric-sample-count">
        {points.length} recorded samples
      </small>
    </div>
  );
}

function logSeverity(row: RecordValue) {
  const label = text(row.severityText);
  if (label) return label;
  const severity = Number(row.severityNumber);
  if (severity >= 21) return "FATAL";
  if (severity >= 17) return "ERROR";
  if (severity >= 13) return "WARN";
  if (severity >= 9) return "INFO";
  if (severity >= 5) return "DEBUG";
  if (severity >= 1) return "TRACE";
  return "—";
}

export function TelemetryPanel({
  kind,
  data,
  loading,
}: {
  kind: "Logs" | "Metrics" | "Events";
  data: unknown;
  loading: boolean;
}) {
  const result = record(data);
  const values = result[kind.toLowerCase()];
  const rows = Array.isArray(values) ? values.map(record) : [];
  const series = kind === "Metrics" ? metricSeries(rows) : [];
  const omitted = Number(result.omitted_count ?? result.omitted ?? 0);
  return (
    <div className={"telemetry-view " + kind.toLowerCase()}>
      {result.gap === true && (
        <p className="notice">
          Some earlier events have expired from the device history.
        </p>
      )}
      {!rows.length ? (
        <p>
          {loading
            ? `Reading ${kind.toLowerCase()}…`
            : `No ${kind.toLowerCase()} in this sample.`}
        </p>
      ) : kind === "Metrics" ? (
        <>
          <p className="telemetry-sample-note">
            Recorded device samples. Charts use the timestamps reported by the
            device.
          </p>
          <div className="metric-grid">
            {series.map((s) => (
              <article className="metric-card" key={s.id}>
                <div className="metric-heading">
                  <h3>{s.name}</h3>
                  {s.labels && <p>{s.labels}</p>}
                </div>
                <strong
                  className="metric-current"
                  title={`${text(s.latestValue)} ${s.unit}`}
                >
                  {(Number(s.latest.flags) & 1) === 0
                    ? metricValue(s.latestValue, s.unit)
                    : "Unavailable"}
                </strong>
                <div className="metric-caption">
                  <span>{s.meaning || "Latest sample"}</span>
                  <time>{timestamp(s.latest)}</time>
                </div>
                <MetricChart series={s} />
              </article>
            ))}
          </div>
        </>
      ) : (
        <>
          {kind === "Logs" && (
            <div className="log-columns" aria-hidden="true">
              <span>Time</span>
              <span>Level</span>
              <span>Message</span>
            </div>
          )}
          <ol className="telemetry-list">
            {rows.map((r, i) => {
              const time = telemetryTime(r);
              return (
                <li key={i}>
                  <time
                    dateTime={
                      time == null ? undefined : new Date(time).toISOString()
                    }
                    title={
                      time == null ? undefined : new Date(time).toLocaleString()
                    }
                  >
                    {timestamp(r)}
                    {time != null && (
                      <small>
                        {new Date(time).toLocaleDateString(undefined, {
                          month: "short",
                          day: "numeric",
                        })}
                      </small>
                    )}
                  </time>
                  {kind === "Events" ? (
                    <div>
                      <strong>{text(r.name).replaceAll("_", " ")}</strong>
                      <small>{text(r.attributes)}</small>
                    </div>
                  ) : (
                    <>
                      <span className="log-level">{logSeverity(r)}</span>
                      <span className="log-message">
                        {text(r.body ?? r.message) || "(Empty log message)"}
                      </span>
                    </>
                  )}
                </li>
              );
            })}
          </ol>
        </>
      )}
      {omitted > 0 && (
        <small className="telemetry-sample-note">
          {omitted} additional records omitted from this sample.
        </small>
      )}
    </div>
  );
}
