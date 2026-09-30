type RecordValue = Record<string, unknown>;
function text(value: unknown): string {
  if (value == null) return "";
  if (typeof value === "object")
    return Object.entries(value)
      .map(([k, v]) => `${k}: ${text(v)}`)
      .join(" · ");
  return String(value);
}
function timestamp(row: RecordValue) {
  const raw =
    row.observed_at ||
    row.timestamp ||
    row.timeUnixNano ||
    row.observedTimeUnixNano;
  if (!raw) return "";
  const date =
    typeof raw === "string" && raw.includes("T")
      ? new Date(raw)
      : new Date(Number(raw) / 1e6);
  return Number.isNaN(date.valueOf()) ? "" : date.toLocaleTimeString();
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
  const result = data as RecordValue | undefined;
  const values = result?.[kind.toLowerCase()];
  const rows = Array.isArray(values) ? (values as RecordValue[]) : [];
  return (
    <div className={"telemetry-view " + kind.toLowerCase()}>
      {result?.gap === true && (
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
        <div className="metric-grid">
          {rows.map((r, i) => (
            <article className="metric-card" key={i}>
              <span>{text(r.name)}</span>
              <strong>
                {text(r.value ?? r.asDouble ?? r.asInt ?? r.sum ?? r.count)}{" "}
                <small>{text(r.unit)}</small>
              </strong>
              <small>{timestamp(r)}</small>
            </article>
          ))}
        </div>
      ) : (
        <ol className="telemetry-list">
          {rows.map((r, i) => (
            <li key={i}>
              <time>{timestamp(r)}</time>
              <div>
                {kind === "Events" ? (
                  <>
                    <strong>{text(r.name).replaceAll("_", " ")}</strong>
                    <small>{text(r.attributes)}</small>
                  </>
                ) : (
                  <>
                    <span className="log-level">{text(r.severityText)}</span>
                    <span className="log-message">
                      {text(r.body ?? r.message)}
                    </span>
                  </>
                )}
              </div>
            </li>
          ))}
        </ol>
      )}
      {Number(result?.omitted_count || 0) > 0 && (
        <small>Showing a limited sample.</small>
      )}
    </div>
  );
}
