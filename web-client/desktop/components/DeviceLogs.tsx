import { useEffect, useId, useMemo, useState } from "react";
import { FileText, Loader2, Pause, Play, RefreshCw, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import type { DeviceLogLevel, DeviceLogSnapshot, DeviceLogSource, DeviceLogsAPI } from "../log-types";
import { monitoringError } from "../monitoring-errors.mjs";

type Props = { api: DeviceLogsAPI; target: string; initialApp?: string; source?: DeviceLogSource };
const errorText = (error: unknown) => error instanceof Error ? error.message : String(error);
const severityName = (value: string) => {
  const name = value.replace(/^severity_number_/i, "").replace(/[2-4]$/, "").toLowerCase();
  if (name === "warning") return "warn";
  if (name === "critical" || name === "panic") return "fatal";
  return ["trace", "debug", "info", "warn", "error", "fatal"].includes(name) ? name : "unknown";
};

export default function DeviceLogs(props: Props) {
  // Switching devices also resets filters and discards the previous device's rows.
  return <DeviceLogStream key={JSON.stringify([props.target, props.initialApp, props.source])} {...props} />;
}

function DeviceLogStream({ api, target, initialApp = "", source }: Props) {
  const inputId = useId();
  const [appInput, setAppInput] = useState(initialApp);
  const [app, setApp] = useState(initialApp);
  const [level, setLevel] = useState<DeviceLogLevel>("all");
  const [search, setSearch] = useState("");
  const [following, setFollowing] = useState(true);
  const [retry, setRetry] = useState(0);
  const [result, setResult] = useState<{ key: string; snapshot?: DeviceLogSnapshot; error?: string }>();
  const sourceKey = JSON.stringify(source || null);
  const key = JSON.stringify([target, app, level, retry, sourceKey]);
  const snapshot = result?.key === key ? result.snapshot : undefined;
  const rawError = result?.key === key ? result.error || snapshot?.error : undefined;
  const error = rawError ? monitoringError(rawError) : undefined;

  useEffect(() => {
    if (!following) return;
    let alive = true;
    let streamId: string | undefined;
    let timer: ReturnType<typeof setTimeout>;
    const stop = () => { if (streamId) void api.deviceLogsStop(streamId).catch(() => {}); };
    const poll = async () => {
      if (!alive || !streamId) return;
      try {
        const next = await api.deviceLogsSnapshot(streamId);
        if (!alive) return;
        // Ignore responses that do not belong to this view's pinned target.
        if (next.id !== streamId || next.target !== target)
          throw new Error("The log stream returned a different device.");
        setResult({ key, snapshot: next });
        if (next.running) timer = setTimeout(() => void poll(), 750);
      } catch (cause) {
        if (alive) {
          setResult((old) => ({ key, snapshot: old?.key === key ? old.snapshot : undefined, error: errorText(cause) }));
          stop();
        }
      }
    };
    void api.deviceLogsStart({ target, app, level, source: JSON.parse(sourceKey) || undefined }).then((stream) => {
      streamId = stream.id;
      if (!alive) { stop(); return; }
      void poll();
    }).catch((cause) => {
      if (alive) setResult({ key, error: errorText(cause) });
    });
    return () => {
      alive = false;
      clearTimeout(timer);
      stop();
    };
  }, [api, target, app, level, following, retry, key, sourceKey]);

  const rows = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return (snapshot?.entries || []).filter((entry) =>
      `${entry.body} ${entry.service} ${entry.severity} ${JSON.stringify(entry.attributes)}`.toLowerCase().includes(needle),
    ).slice().reverse();
  }, [snapshot, search]);
  const ended = !!snapshot && !snapshot.running;
  const status = !following ? "Paused" : error ? "Disconnected" : ended ? "Stream ended" : !snapshot ? "Connecting…" : "Following";
  const reconnect = () => { setRetry((n) => n + 1); setFollowing(true); };

  return (
    <section className="desktop-card desktop-device-logs" aria-label={`Logs for ${target}`}>
      <div className="desktop-section-heading">
        <div>
          <h2><FileText size={18} aria-hidden="true" /> Logs</h2>
          <p className="desktop-muted">{target}{source ? "" : app ? ` · ${app}` : " · All applications"}</p>
        </div>
        <div className="desktop-actions">
          <span className={error ? "desktop-failed" : following && !ended ? "desktop-connected" : "desktop-muted"} role="status">
            {!snapshot && following && !error && <Loader2 size={14} className="animate-spin" aria-hidden="true" />} {status}
          </span>
          {error || ended ? (
            <Button variant="outline" size="sm" onClick={reconnect}><RefreshCw /> Reconnect</Button>
          ) : (
            <Button variant="outline" size="sm" onClick={() => setFollowing((value) => !value)}>
              {following ? <Pause /> : <Play />} {following ? "Pause" : "Resume"}
            </Button>
          )}
        </div>
      </div>
      <div className="desktop-log-controls desktop-padding">
        {!source && <form onSubmit={(event) => {
          event.preventDefault();
          setApp(appInput.trim());
          setFollowing(true);
        }}>
          <Label htmlFor={inputId}>Application</Label>
          <Input id={inputId} value={appInput} maxLength={256} onChange={(event) => setAppInput(event.target.value)} placeholder="All applications" />
          <Button type="submit" variant="outline" size="sm" disabled={app === appInput.trim()}>Apply</Button>
        </form>}
        {!source && <label className="desktop-log-level">
          <span>Severity</span>
          <select aria-label="Log severity" value={level} onChange={(event) => { setLevel(event.target.value as DeviceLogLevel); setFollowing(true); }}>
            <option value="all">All levels</option>
            <option value="trace">Trace &amp; above</option>
            <option value="debug">Debug &amp; above</option>
            <option value="info">Info &amp; above</option>
            <option value="warn">Warning &amp; above</option>
            <option value="error">Error &amp; above</option>
            <option value="fatal">Fatal</option>
          </select>
        </label>}
        <label className="desktop-log-search">
          <Search size={16} aria-hidden="true" />
          <Input aria-label="Search logs" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search messages, services, attributes…" />
        </label>
      </div>
      {error && <p role="alert" className="desktop-error desktop-padding">{error}</p>}
      <div className="desktop-log-summary desktop-muted">
        <span>{rows.length} {rows.length === 1 ? "entry" : "entries"}{search && snapshot ? ` of ${snapshot.entries.length}` : ""} · Newest first</span>
        <span>{snapshot?.dropped ? `${snapshot.dropped} entries omitted` : "Up to 500 recent entries"}</span>
      </div>
      <div className="desktop-log-entries" aria-label="Log entries">
        {rows.map((entry) => {
          const severity = severityName(entry.severity);
          return (
            <details className="desktop-log-entry" data-severity={severity} key={entry.id}>
              <summary>
                <time dateTime={entry.timestamp} title={entry.timestamp}>{new Date(entry.timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false, fractionalSecondDigits: 3 })}</time>
                <span className="desktop-log-severity">{severity === "unknown" ? entry.severity || "LOG" : severity.toUpperCase()}</span>
                <span className="desktop-log-service">{entry.service || "Device"}</span>
                <span className="desktop-log-message">{entry.body || "(Empty message)"}</span>
              </summary>
              <div className="desktop-log-detail">
                <time dateTime={entry.timestamp}>{new Date(entry.timestamp).toLocaleString()}</time>
                <p>{entry.body || "(Empty message)"}</p>
                {Object.keys(entry.attributes).length > 0 && <dl>{Object.entries(entry.attributes).map(([name, value]) =>
                  <div key={name}><dt>{name}</dt><dd>{value}</dd></div>,
                )}</dl>}
              </div>
            </details>
          );
        })}
        {!rows.length && (
          <div className="desktop-log-empty">
            <FileText size={28} aria-hidden="true" />
            <h3>{error ? "Logs unavailable" : !following ? "Log updates paused" : snapshot?.entries.length ? "No matching entries" : ended ? "No log entries received" : "Waiting for log entries"}</h3>
            <p className="desktop-muted">{error ? "Reconnect to try this log source again." : !following ? "Resume to load recent entries and follow new logs." : snapshot?.entries.length ? "Try a different search." : source ? "New messages appear here automatically." : "Application and agent messages appear here as the device emits them. You can adjust the application or severity filter."}</p>
          </div>
        )}
      </div>
    </section>
  );
}
