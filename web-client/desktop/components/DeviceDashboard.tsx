import { useEffect, useRef, useState } from "react";
import { Activity, Cpu, Database, HardDrive, Pause, Play, RefreshCw, Thermometer } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { DesktopAPI, DeviceSnapshot } from "../types";
import { monitoringError } from "../monitoring-errors.mjs";

const known = (value: number | undefined): value is number => typeof value === "number" && Number.isFinite(value);
const percent = (value: number | undefined) => known(value) ? `${value.toFixed(1)}%` : "Unavailable";
const bytes = (value: number | undefined) => {
  if (!known(value)) return "Unavailable";
  if (value === 0) return "0 B";
  const unit = Math.min(4, Math.max(0, Math.floor(Math.log(value) / Math.log(1024))));
  return `${(value / 1024 ** unit).toFixed(unit ? 1 : 0)} ${["B", "KiB", "MiB", "GiB", "TiB"][unit]}`;
};

function Trend({ values }: { values: number[] }) {
  if (values.length < 2) return <div className="desktop-metric-trend" />;
  const points = values.map((value, index) => `${index * 240 / (values.length - 1)},${44 - Math.min(100, Math.max(0, value)) * 0.4}`).join(" ");
  return <svg className="desktop-metric-trend" viewBox="0 0 240 48" preserveAspectRatio="none" aria-hidden="true"><polyline points={points} fill="none" stroke="currentColor" strokeWidth="2" vectorEffect="non-scaling-stroke" /></svg>;
}

export default function DeviceDashboard({ api, target, onLogs }: { api: DesktopAPI; target: string; onLogs: (app?: string) => void }) {
  const [snapshot, setSnapshot] = useState<DeviceSnapshot>();
  const [history, setHistory] = useState<{ cpu: number[]; memory: number[] }>({ cpu: [], memory: [] });
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [paused, setPaused] = useState(false);
  const [generation, setGeneration] = useState(0);
  const inflight = useRef<Promise<DeviceSnapshot> | null>(null);
  const previousGeneration = useRef(0);
  useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function update() {
      try {
        // Share the request across effect refreshes, including development's
        // StrictMode cycle. A target change mounts a fresh dashboard.
        const request = inflight.current || api.deviceSnapshot({ target });
        inflight.current = request;
        const next = await request;
        if (disposed) return;
        if (next.target !== target) throw new Error("The dashboard returned a different device.");
        setSnapshot(next);
        setError("");
        setHistory((previous) => ({
          cpu: known(next.host.cpuPercent) ? [...previous.cpu, next.host.cpuPercent].slice(-60) : previous.cpu,
          memory: known(next.host.memUsedBytes) && next.host.memTotalBytes && next.host.memTotalBytes > 0 ? [...previous.memory, next.host.memUsedBytes / next.host.memTotalBytes * 100].slice(-60) : previous.memory,
        }));
      } catch (error) {
        if (!disposed) setError(monitoringError(error));
      } finally {
        inflight.current = null;
        if (!disposed) {
          setLoading(false);
          if (!paused) timer = setTimeout(() => void update(), 2000);
        }
      }
    }
    const refreshRequested = generation !== previousGeneration.current;
    previousGeneration.current = generation;
    if (!paused || refreshRequested) void update();
    return () => { disposed = true; clearTimeout(timer); };
  }, [api, target, paused, generation]);
  const host = snapshot?.host;
  const storage = host?.containerStorage;
  const memoryKnown = known(host?.memUsedBytes) && known(host?.memTotalBytes) && host.memTotalBytes > 0;
  const temperature = host?.maximumTemperature;
  return (
    <section className="desktop-dashboard" aria-label={`Dashboard for ${target}`}>
      <div className="desktop-section-heading">
        <div><Activity size={18} /><h2>{target}</h2><span className={error ? "desktop-failed" : paused ? "desktop-muted" : "desktop-connected"}>{error ? "Connection interrupted" : paused ? "Paused" : loading ? "Connecting…" : "Live"}</span></div>
        <div className="desktop-actions">
          {snapshot && <span className="desktop-muted">Updated {new Date(snapshot.sampledAt).toLocaleTimeString()}</span>}
          <Button size="sm" variant="ghost" onClick={() => setPaused(!paused)}>{paused ? <Play /> : <Pause />}{paused ? "Resume" : "Pause"}</Button>
          <Button size="sm" variant="outline" onClick={() => setGeneration((n) => n + 1)}><RefreshCw />Refresh</Button>
        </div>
      </div>
      {error && <p className="desktop-error" role="alert">{error}{snapshot ? " Last reported values are shown below." : ""}</p>}
      {!snapshot ? <p className="desktop-muted desktop-padding">{loading ? "Reading device resources…" : "Device resources are unavailable until a connection is established."}</p> : <>
        <div className="desktop-metrics">
          <article className="desktop-metric"><span><Cpu size={16} />CPU</span><strong>{percent(host?.cpuPercent)}</strong><small>{host?.cpuCount ? `${host.cpuCount} CPU cores` : "Core count unavailable"}</small><Trend values={history.cpu} /></article>
          <article className="desktop-metric"><span><Database size={16} />Memory</span><strong>{memoryKnown ? bytes(host.memUsedBytes) : "Unavailable"}</strong><small>{memoryKnown ? `of ${bytes(host.memTotalBytes)}` : "Not reported by this device"}</small><Trend values={history.memory} /></article>
          <article className="desktop-metric"><span><HardDrive size={16} />Container storage</span><strong>{storage && storage.totalBytes > 0 ? bytes(storage.usedBytes) : "Unavailable"}</strong><small>{storage && storage.totalBytes > 0 ? `of ${bytes(storage.totalBytes)} · ${storage.mountpoint}` : "Not reported by this device"}</small></article>
          <article className="desktop-metric"><span><Thermometer size={16} />Temperature</span><strong>{temperature && known(temperature.tempC) ? `${temperature.tempC.toFixed(1)}°C` : "Unavailable"}</strong><small>{temperature?.name || "Not reported by this device"}</small></article>
        </div>
        {!!host?.gpus?.length && <div className="desktop-metrics desktop-metrics-secondary">{host.gpus.map((gpu) => <article className="desktop-metric" key={gpu.index}><span>{gpu.name || `GPU ${gpu.index}`}</span><strong>{percent(gpu.utilPercent)}</strong><small>{gpu.memTotalBytes ? `${bytes(gpu.memUsedBytes)} of ${bytes(gpu.memTotalBytes)}` : "GPU memory unavailable"}{known(gpu.tempC) ? ` · ${gpu.tempC.toFixed(1)}°C` : ""}{known(gpu.powerW) ? ` · ${gpu.powerW.toFixed(1)} W` : ""}</small></article>)}</div>}
        {host?.battery && <p className="desktop-padding desktop-muted">Battery {percent(host.battery.percent)} · {host.battery.state}</p>}
        <div className="desktop-section-heading"><div><h3>Applications</h3><span className="desktop-muted">{snapshot?.containers.length || 0} reported</span></div><Button size="sm" variant="ghost" onClick={() => onLogs()}>All logs</Button></div>
        <div className="desktop-table-wrap"><table><thead><tr><th>Application</th><th>State</th><th>CPU</th><th>Memory</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{snapshot?.containers.map((container) => <tr key={container.name}><td>{container.name}</td><td><span className={container.state.toLowerCase() === "running" ? "desktop-connected" : "desktop-muted"}>{container.state}</span></td><td>{percent(container.cpuPercent)}</td><td>{bytes(container.memBytes)}</td><td><Button size="sm" variant="ghost" onClick={() => onLogs(container.name.replace(/ \[group\]$/, ""))}>Logs</Button></td></tr>)}</tbody></table></div>
        {snapshot && !snapshot.containers.length && <p className="desktop-padding desktop-muted">No applications are running on this device.</p>}
      </>}
    </section>
  );
}
