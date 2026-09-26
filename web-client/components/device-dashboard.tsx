"use client";
import { useEffect, useRef, useState } from "react";
import {
  ArrowLeft,
  Activity,
  Loader2,
  RefreshCw,
  Play,
  Square,
  RotateCw,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { WendyClient, formatBytes } from "@/lib/client";
import {
  usage,
  telemetryRows,
  meetsLogLevel,
  type Sample,
  type Reading,
  type OTelBatch,
  type TelemetryRow,
} from "@/lib/telemetry";
type Tab = "top" | "logs" | "metrics" | "traces";
export default function DeviceDashboard({
  device,
  client,
  onBack,
  requestedView,
}: {
  device: { id: string; name: string };
  client: WendyClient;
  onBack: () => void;
  requestedView: string;
}) {
  const [tab, setTab] = useState<Tab>("top");
  const [connected, setConnected] = useState(false);
  const [connecting, setConnecting] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [reading, setReading] = useState<Reading | null>(null);
  const [load, setLoad] = useState<ReturnType<typeof usage>>({
    cpu: null,
    containers: new Map(),
  });
  const [history, setHistory] = useState<number[]>([]);
  const [rows, setRows] = useState<TelemetryRow[]>([]);
  const [streamError, setStreamError] = useState("");
  const [app, setApp] = useState("");
  const [search, setSearch] = useState("");
  const [severity, setSeverity] = useState("info");
  const [sort, setSort] = useState<"cpu" | "memory">("cpu");
  const [acting, setActing] = useState("");
  const previous = useRef<Reading | null>(null);
  useEffect(() => {
    const name = requestedView.toLowerCase();
    setTab(
      name === "logs" || name === "metrics" || name === "traces" ? name : "top",
    );
  }, [requestedView]);
  useEffect(() => {
    let alive = true,
      timer: ReturnType<typeof setTimeout>;
    setConnecting(true);
    setConnected(false);
    setError("");
    setReading(null);
    setHistory([]);
    previous.current = null;
    async function poll() {
      try {
        const sample = await client.call<Sample>("snapshot");
        if (!alive) return;
        const next = { sample, at: performance.now() },
          nextLoad = usage(previous.current, next);
        previous.current = next;
        setReading(next);
        setLoad(nextLoad);
        setError("");
        if (nextLoad.cpu !== null)
          setHistory((h) => [...h, nextLoad.cpu!].slice(-60));
      } catch (e) {
        if (alive) setError(String(e));
      }
      if (alive) timer = setTimeout(poll, 2000);
    }
    client
      .call("cloud-connect", { id: device.id })
      .then(() => {
        if (alive) {
          setConnecting(false);
          setConnected(true);
          void poll();
        }
      })
      .catch((e) => {
        if (alive) {
          setConnecting(false);
          setError(String(e));
        }
      });
    return () => {
      alive = false;
      clearTimeout(timer);
      void client.call("disconnect").catch(() => {});
    };
  }, [client, device.id, retry]);
  useEffect(() => {
    setRows([]);
    setStreamError("");
    if (!connected || tab === "top") return;
    let alive = true,
      seq = 0;
    const off = client.onEvent((event) => {
      if (!alive) return;
      if (event.event === tab + "-error") setStreamError(String(event.data));
      if (event.event !== tab) return;
      const incoming = telemetryRows(tab, event.data as OTelBatch).map((r) => ({
        ...r,
        key: r.key || String(++seq),
      }));
      setRows((old) => {
        if (tab === "logs")
          return [...incoming.reverse(), ...old].slice(0, 500);
        const map = new Map(old.map((r) => [r.key, r]));
        for (const r of incoming) {
          map.delete(r.key);
          map.set(r.key, r);
        }
        return Array.from(map.values()).slice(-500);
      });
    });
    client.call(tab, { app }).catch((e) => {
      if (alive) setStreamError(String(e));
    });
    return () => {
      alive = false;
      off();
      void client.call("telemetry-stop").catch(() => {});
    };
  }, [client, connected, tab, app]);
  const shownRows = rows.filter((r) =>
    (tab !== "logs" || meetsLogLevel(r.severity, severity)) &&
    `${r.name} ${r.value} ${r.service} ${r.trace || ""}`
      .toLowerCase().includes(search.toLowerCase()),
  );
  const sample = reading?.sample,
    host = sample?.stats?.host;
  const disk =
    sample?.version.containerStorage ||
    sample?.version.partitions?.find((p) => p.mountpoint === "/data");
  const memory = host?.memTotalBytes
    ? Number(host.memTotalBytes) - Number(host.memAvailableBytes || 0)
    : null;
  const totalMemory = host?.memTotalBytes ? Number(host.memTotalBytes) : null;
  const temperatures = [
    ...(host?.thermalZones || []).map((z) => z.tempC ?? 0),
    ...(host?.gpus || []).map((g) => g.tempC),
  ].filter((t): t is number => t !== undefined);
  const apps = (sample?.apps || [])
    .map((a) => {
      const services =
        a.services && a.services.length > 1
          ? a.services.map((s) => ({
              ...s,
              ...load.containers.get(a.appName + "_" + s.name),
            }))
          : [];
      const own = load.containers.get(a.appName);
      return {
        ...a,
        services,
        cpu: services.length
          ? services.every((s) => s.cpu !== null && s.cpu !== undefined)
            ? services.reduce((sum, s) => sum + (s.cpu || 0), 0)
            : null
          : own?.cpu,
        memory: services.length
          ? services.reduce((sum, s) => sum + (s.memory || 0), 0)
          : own?.memory,
      };
    })
    .sort((a, b) => (b[sort] || 0) - (a[sort] || 0));
  async function action(method: string, name: string) {
    setActing(name);
    setError("");
    try {
      await client.call(method, { app: name });
    } catch (e) {
      setError(String(e));
    } finally {
      setActing("");
    }
  }
  return (
    <section
      className="cloud-dashboard"
      aria-label={`${device.name || "Device"} dashboard`}
    >
      <button className="device-back" onClick={onBack}>
        <ArrowLeft size={15} /> Online cloud devices
      </button>
      <div className="page-heading">
        <div>
          <p className="eyebrow">CLOUD DEVICE</p>
          <h1>{device.name || "Unnamed device"}</h1>
          <p className="subtle">
            {connecting
              ? "Establishing a secure device session…"
              : connected
                ? `${sample?.version.version ? "Wendy " + sample.version.version + " · " : ""}Live resource and application telemetry`
                : "Device session unavailable"}
          </p>
        </div>
        <span className={`tag ${connected && !error ? "live" : ""}`}>
          {connecting ? (
            <Loader2 size={14} className="animate-spin" />
          ) : (
            <Activity size={14} />
          )}
          {connecting
            ? "Connecting"
            : error
              ? "Connection interrupted"
              : "Live"}
        </span>
      </div>
      {error && (
        <div className="inline-error" role="alert">
          {error}
          <Button variant="ghost" onClick={() => setRetry((r) => r + 1)}>
            <RefreshCw /> Reconnect
          </Button>
        </div>
      )}
      <div className="device-tabs" aria-label="Device views">
        {(["top", "logs", "metrics", "traces"] as const).map((t) => (
          <button key={t} onClick={() => setTab(t)} aria-pressed={tab === t}>
            {t === "top" ? "Dashboard" : t[0].toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>
      {connecting && (
        <div className="device-wait">
          <Loader2 className="animate-spin" />
          <p>Connecting through Wendy Cloud</p>
        </div>
      )}
      {connected && tab === "top" && (
        <>
          <div className="top-metrics">
            <Meter
              label="CPU"
              value={
                load.cpu === null ? "Sampling…" : load.cpu.toFixed(1) + "%"
              }
              note={
                host?.cpuCount
                  ? `${host.cpuCount} cores`
                  : "Waiting for host metrics"
              }
              percent={load.cpu}
            />
            <Meter
              label="Memory"
              value={formatBytes(memory)}
              note={
                totalMemory ? `of ${formatBytes(totalMemory)}` : "Not reported"
              }
              percent={
                memory !== null && totalMemory
                  ? (memory / totalMemory) * 100
                  : null
              }
            />
            <Meter
              label="Container storage"
              value={disk ? formatBytes(Number(disk.usedBytes || 0)) : "—"}
              note={
                disk
                  ? `of ${formatBytes(Number(disk.totalBytes || 0))} · ${disk.mountpoint || ""}`
                  : "Not reported"
              }
              percent={
                Number(disk?.totalBytes)
                  ? (Number(disk?.usedBytes || 0) / Number(disk?.totalBytes)) *
                    100
                  : null
              }
            />
            <Meter
              label="Temperature"
              value={
                temperatures.length
                  ? Math.max(...temperatures).toFixed(1) + "°C"
                  : "—"
              }
              note={
                temperatures.length ? "Hottest reported sensor" : "Not reported"
              }
              percent={null}
            />
          </div>
          {!!sample?.warnings?.length && (
            <div className="inline-error">{sample.warnings.join(" · ")}</div>
          )}
          <section className="panel cpu-chart">
            <div className="panel-heading">
              <h2>CPU activity</h2>
              <span className="subtle">
                Latest 60 samples · every 2 seconds
              </span>
            </div>
            {history.length > 1 ? (
              <svg
                role="img"
                aria-label="Live CPU usage, zero to one hundred percent"
                viewBox="0 0 600 110"
                preserveAspectRatio="none"
              >
                <path
                  d="M0 100 H600 M0 50 H600 M0 0 H600"
                  stroke="currentColor"
                  opacity="0.12"
                  fill="none"
                />
                <polyline
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="2"
                  points={history
                    .map(
                      (v, i) =>
                        `${(i / (history.length - 1)) * 600},${100 - v}`,
                    )
                    .join(" ")}
                />
              </svg>
            ) : (
              <p className="subtle p-5">Waiting for two CPU samples…</p>
            )}
          </section>
          {((host?.gpus?.length || 0) > 0 || host?.battery) && (
            <div className="top-metrics">
              {host?.gpus?.map((g, i) => (
                <Meter
                  key={i}
                  label={g.name || `GPU ${i}`}
                  value={(g.utilPercent ?? 0).toFixed(1) + "%"}
                  note={`${Number(g.memTotalBytes) ? formatBytes(Number(g.memUsedBytes || 0)) + " / " + formatBytes(Number(g.memTotalBytes)) : "Shared memory"}${g.tempC !== undefined ? " · " + g.tempC + "°C" : ""}`}
                  percent={g.utilPercent ?? 0}
                />
              ))}
              {host?.battery && (
                <Meter
                  label="Battery"
                  value={`${host.battery.percent ?? 0}%`}
                  note={(host.battery.state || "")
                    .replace("BATTERY_STATE_", "")
                    .toLowerCase()}
                  percent={host.battery.percent ?? null}
                />
              )}
            </div>
          )}
          <section className="panel">
            <div className="panel-heading">
              <h2>
                Applications <span className="subtle">/ {apps.length}</span>
              </h2>
              <label className="subtle">
                Sort by{" "}
                <select
                  value={sort}
                  onChange={(e) => setSort(e.target.value as "cpu" | "memory")}
                >
                  <option value="cpu">CPU</option>
                  <option value="memory">Memory</option>
                </select>
              </label>
            </div>
            <div className="device-table-wrap">
              <table className="device-table">
                <thead>
                  <tr>
                    <th>Application</th>
                    <th>State</th>
                    <th>CPU</th>
                    <th>Memory</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {apps.map((a) => (
                    <tr key={a.appName}>
                      <td>
                        <button
                          className="app-link"
                          onClick={() => {
                            setApp(a.appName);
                            setTab("logs");
                          }}
                        >
                          {a.appName}
                        </button>
                        {a.services.map((s) => (
                          <div className="subtle service-stat" key={s.name}>
                            {s.name} ·{" "}
                            {s.cpu == null ? "—" : s.cpu.toFixed(1) + "%"} ·{" "}
                            {formatBytes(s.memory ?? null)}
                          </div>
                        ))}
                      </td>
                      <td>
                        <span
                          className={`tag ${a.runningState === "RUNNING" ? "live" : ""}`}
                        >
                          {(a.runningState || "unknown")
                            .toLowerCase()
                            .replaceAll("_", " ")}
                        </span>
                      </td>
                      <td>{a.cpu == null ? "—" : a.cpu.toFixed(1) + "%"}</td>
                      <td>{formatBytes(a.memory ?? null)}</td>
                      <td>
                        <div className="actions">
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={!!acting}
                            onClick={() =>
                              void action(
                                a.runningState === "RUNNING" ? "stop" : "start",
                                a.appName,
                              )
                            }
                          >
                            {a.runningState === "RUNNING" ? (
                              <Square size={14} />
                            ) : (
                              <Play size={14} />
                            )}{" "}
                            {a.runningState === "RUNNING" ? "Stop" : "Start"}
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={!!acting}
                            onClick={() => void action("restart", a.appName)}
                          >
                            <RotateCw size={14} /> Restart
                          </Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {!apps.length && (
                <p className="subtle p-5">
                  {reading
                    ? "No applications reported by this device."
                    : "Loading applications…"}
                </p>
              )}
            </div>
          </section>
        </>
      )}
      {connected && tab !== "top" && (
        <section className="panel telemetry-panel">
          <div className="panel-heading">
            <div>
              <h2>OpenTelemetry {tab}</h2>
              <p className="subtle">
                {tab === "metrics"
                  ? "Latest values per metric and attribute set"
                  : "Recent history and live events"}{" "}
                · up to 500 entries
              </p>
            </div>
            <div className="telemetry-controls">
              {tab === "logs" && (
                <select
                  aria-label="Log severity"
                  value={severity}
                  onChange={(e) => setSeverity(e.target.value)}
                >
                  <option value="all">All levels</option>
                  <option value="info">Info &amp; above</option>
                  <option value="warn">Warning &amp; above</option>
                  <option value="error">Errors only</option>
                </select>
              )}
              <select
                aria-label="Filter by application"
                value={app}
                onChange={(e) => setApp(e.target.value)}
              >
                <option value="">All applications</option>
                {apps.map((a) => (
                  <option key={a.appName} value={a.appName}>
                    {a.appName}
                  </option>
                ))}
              </select>
              <input
                aria-label={`Search ${tab}`}
                placeholder={`Search ${tab}…`}
                value={search}
                onChange={(e) => setSearch(e.target.value)}
              />
            </div>
          </div>
          {streamError && (
            <div className="inline-error" role="alert">
              {streamError}
            </div>
          )}
          <div className="telemetry-rows">
            {shownRows
              .map((r) => (
                <details key={r.key} className="telemetry-row" data-severity={r.severity}>
                  <summary>
                    <time>{r.time}</time>
                    <span className="telemetry-service">{r.service}</span>
                    <strong>{r.name}</strong>
                    <span
                      className={
                        r.status === "STATUS_CODE_ERROR" ? "text-red-400" : ""
                      }
                    >
                      {r.value}
                    </span>
                  </summary>
                  {r.trace && <p className="subtle">Trace {r.trace}</p>}
                  <pre>{JSON.stringify(r.detail, null, 2)}</pre>
                </details>
              ))}
          </div>
          {!shownRows.length && (
            <div className="device-wait">
              <Activity />
              <h3>
                {streamError ? "Stream unavailable" : rows.length ? "No matching entries" : `Waiting for ${tab}`}
              </h3>
              <p className="subtle">
                {streamError
                  ? "The device may be offline or its agent may need an update."
                  : rows.length ? "Adjust your search or log severity filter." : `Applications on this device must emit OpenTelemetry ${tab}. New data appears here automatically.`}
              </p>
            </div>
          )}
        </section>
      )}
    </section>
  );
}
function Meter({
  label,
  value,
  note,
  percent,
}: {
  label: string;
  value: string;
  note: string;
  percent: number | null;
}) {
  return (
    <section className="panel top-meter">
      <span className="eyebrow">{label}</span>
      <strong>{value}</strong>
      <p className="subtle">{note}</p>
      {percent !== null && (
        <div className="meter-track">
          <span style={{ width: Math.max(0, Math.min(100, percent)) + "%" }} />
        </div>
      )}
    </section>
  );
}
