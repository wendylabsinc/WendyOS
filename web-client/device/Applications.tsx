import { useEffect, useRef, useState } from "react";
import { Box, Cpu, MemoryStick, Play, Square, RotateCw, ArrowUpRight } from "lucide-react";
import { Line, LineChart, ResponsiveContainer, YAxis } from "recharts";
import type { WendyClient } from "@/lib/client";
import { usage, type Sample, type Reading } from "@/lib/telemetry";
import { metricServiceLabel } from "@/lib/metric-charts";
import { bytes, errorMessage } from "./presentation";
import { sections } from "./operations";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import Result from "./Result";
import { failure } from "./rpc";
import "./Applications.css";

type AppItem = Sample["apps"][number] & { appVersion?: string; imageName?: string };
type AppStat = { cpu: number | null; memory: number | null; history: { at: number; cpu: number | null }[] };
function ApplicationPorts({ client, appName, refresh }: { client: WendyClient; appName: string; refresh: number }) {
  const [state, setState] = useState<{ ports?: { port: number; protocol: string; address: string }[]; error?: string }>({});
  useEffect(() => {
    let alive = true;
    client.call<{ ports?: { port: number; protocol: string; address: string }[] }>("device-rpc", { service: "wendy.agent.services.v1.WendyContainerService", method: "GetContainerPorts", message: { appName } }).then((value) => {
      if (alive) setState(failure(value) ? { error: failure(value) } : { ports: value.ports ?? [] });
    }).catch((error) => { if (alive) setState({ error: errorMessage(error) }); });
    return () => { alive = false; };
  }, [client, appName, refresh]);
  return <section className="app-detail-section"><h3>Listening ports</h3>{state.error ? <p className="inline-error" role="alert">{state.error}</p> : !state.ports ? <p className="subtle">Reading ports…</p> : state.ports.length ? <table className="application-ports"><thead><tr><th>Port</th><th>Protocol</th><th>Address</th></tr></thead><tbody>{state.ports.map((port) => <tr key={`${port.protocol}-${port.address}-${port.port}`}><td>{port.port}</td><td>{port.protocol.toUpperCase()}</td><td>{port.address}</td></tr>)}</tbody></table> : <p className="subtle">No listening ports.</p>}</section>;
}
export default function Applications({ client, refresh, onLogs }: { client: WendyClient; refresh: number; onLogs?: (app: string) => void }) {
  const [sample, setSample] = useState<Sample>(), [stats, setStats] = useState<Record<string, AppStat>>({});
  const [error, setError] = useState(""), [selected, setSelected] = useState(""), [query, setQuery] = useState("");
  const [acting, setActing] = useState(""), [confirm, setConfirm] = useState(""), [revision, setRevision] = useState(0);
  const previous = useRef<Reading | null>(null);
  useEffect(() => {
    let alive = true, timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const next = await client.call<Sample>("snapshot"); if (!alive) return;
        const reading = { sample: next, at: performance.now() }, load = usage(previous.current, reading);
        previous.current = reading; setSample(next); setError("");
        setStats((old) => Object.fromEntries(next.apps.map((app) => {
          const children = app.services && app.services.length > 1 ? app.services.map((s) => load.containers.get(app.appName + "_" + s.name)) : [load.containers.get(app.appName)];
          const cpu = children.every((s) => s?.cpu != null) ? children.reduce((sum, s) => sum + (s?.cpu ?? 0), 0) : null;
          const memory = children.every((s) => s?.memory != null) ? children.reduce((sum, s) => sum + (s?.memory ?? 0), 0) : null;
          return [app.appName, { cpu, memory, history: [...(old[app.appName]?.history ?? []), { at: Date.now(), cpu }].slice(-60) }];
        })));
      } catch (e) { if (alive) setError(errorMessage(e)); }
      if (alive) timer = setTimeout(poll, 2000);
    };
    void poll(); return () => { alive = false; clearTimeout(timer); };
  }, [client, refresh, revision]);
  const ops = sections.find((s) => s.name === "Applications")!.operations;
  const apps = (sample?.apps ?? []) as AppItem[];
  const app = apps.find((item) => item.appName === selected);
  const visible = apps.filter((item) => `${item.appName} ${metricServiceLabel(item.appName)}`.toLowerCase().includes(query.toLowerCase()));
  async function perform(method: string) {
    setActing(method); setError("");
    try { await client.call(method, { app: selected }); setConfirm(""); setRevision((n) => n + 1); }
    catch (e) { setError(errorMessage(e)); } finally { setActing(""); }
  }
  function select(item: AppItem) { setSelected(item.appName); setConfirm(""); }
  const running = apps.filter((item) => item.runningState === "RUNNING").length;
  return <div className="applications-view">
    <div className="applications-toolbar"><div><span className="app-status running"><i />{running} running</span><span className="subtle">{apps.length} installed</span></div><div className="applications-tools"><input aria-label="Search applications" placeholder="Find an application…" value={query} onChange={(e) => setQuery(e.target.value)} /><OperationCard client={client} operation={ops.find((op) => op.method === "CreateContainer")!} refresh={refresh} presentation="header" triggerLabel="Add application" onApplied={() => setRevision((n) => n + 1)} /></div></div>
    {error && !app && <p className="inline-error" role="alert">{error}</p>}
    {sample?.warnings?.map((warning) => <p key={warning} className="inline-error" role="alert">{warning}</p>)}
    <div className="application-grid">{visible.map((item) => {
      const stat = stats[item.appName], running = item.runningState === "RUNNING";
      return <article className="panel application-card" key={item.appName}><header><div className="app-icon"><Box size={25} /></div><div><h2>{metricServiceLabel(item.appName)}</h2><span className={`app-status ${running ? "running" : "stopped"}`}><i />{running ? "Running" : (item.runningState || "Unknown").toLowerCase()}</span></div><button className="quiet" aria-label={`Manage ${metricServiceLabel(item.appName)}`} onClick={() => select(item)}><ArrowUpRight size={16} />Manage</button></header>
        <p className="application-id">{item.appName}</p>
        <div className="application-stats"><div><span><Cpu size={15} />CPU</span><strong>{stat?.cpu == null ? "—" : `${stat.cpu.toFixed(1)}%`}</strong></div><div><span><MemoryStick size={15} />Memory</span><strong>{stat?.memory == null ? "—" : bytes(stat.memory)}</strong></div></div>
        <div className="application-spark" role="img" aria-label={`${metricServiceLabel(item.appName)} CPU usage over the last two minutes`}><ResponsiveContainer width="100%" height={64}><LineChart data={stat?.history ?? []}><YAxis hide domain={[0, 100]} /><Line dataKey="cpu" type="linear" stroke="#9FE2BF" strokeWidth={2} dot={false} isAnimationActive={false} connectNulls={false} /></LineChart></ResponsiveContainer></div>
        <footer><span>{item.appVersion ? `Version ${item.appVersion}` : ""}</span><span>{running ? "Live resource usage" : "Not running"}</span></footer>
      </article>;
    })}</div>
    {!visible.length && <div className="application-empty"><Box size={38} /><h2>{!sample ? "Loading applications…" : query ? "No matching applications" : "No applications installed"}</h2>{!query && sample && <p>Add an application to get started.</p>}</div>}
    <Sheet open={Boolean(app)} title={app ? metricServiceLabel(app.appName) : "Application"} description={app?.appName} onClose={() => { if (!acting) { setSelected(""); setConfirm(""); } }}>
      {app && <><div className="app-sheet-status"><span className={`app-status ${app.runningState === "RUNNING" ? "running" : "stopped"}`}><i />{app.runningState === "RUNNING" ? "Running" : (app.runningState || "Unknown").toLowerCase()}</span></div>
        <div className="application-stats"><div><span><Cpu size={15} />CPU</span><strong>{stats[selected]?.cpu == null ? "—" : `${stats[selected].cpu!.toFixed(1)}%`}</strong></div><div><span><MemoryStick size={15} />Memory</span><strong>{stats[selected]?.memory == null ? "—" : bytes(stats[selected].memory)}</strong></div></div>
        {error && <p className="inline-error" role="alert">{error}</p>}
        <div className="app-sheet-actions"><button className="quiet" disabled={Boolean(acting)} onClick={() => setConfirm(app.runningState === "RUNNING" ? "stop" : "start")}>{app.runningState === "RUNNING" ? <Square size={16} /> : <Play size={16} />}{app.runningState === "RUNNING" ? "Stop" : "Start"}</button><button className="quiet" disabled={Boolean(acting) || app.runningState !== "RUNNING"} onClick={() => setConfirm("restart")}><RotateCw size={16} />Restart</button>{onLogs && <button className="quiet" onClick={() => onLogs(selected)}>View logs</button>}</div>
        {confirm && <div className="device-review"><p>{confirm[0].toUpperCase() + confirm.slice(1)} {metricServiceLabel(app.appName)}?</p>{app.appName === "sh.wendy.device-panel" && <p className="subtle">This will disconnect the control panel.</p>}<div className="actions"><button className="primary" disabled={Boolean(acting)} onClick={() => void perform(confirm)}>{acting ? "Working…" : `Confirm ${confirm}`}</button><button className="quiet" disabled={Boolean(acting)} onClick={() => setConfirm("")}>Cancel</button></div></div>}
        <section className="app-detail-section"><h3>Application details</h3><dl className="device-result"><div><dt>Version</dt><dd>{app.appVersion || "Not reported"}</dd></div>{app.imageName && <div><dt>Image</dt><dd>{app.imageName}</dd></div>}</dl>{app.services && app.services.length > 1 && <Result value={app.services} />}</section>
        <ApplicationPorts key={selected} client={client} appName={selected} refresh={revision} />
        <section className="app-detail-section application-danger"><div className="app-section-heading"><h3>Remove application</h3><OperationCard client={client} operation={{ ...ops.find((op) => op.method === "DeleteContainer")!, initial: { appName: selected } }} refresh={revision} presentation="header" triggerLabel="Remove" onApplied={() => { setSelected(""); setRevision((n) => n + 1); }} /></div><p className="subtle">Removes this application. Deleting its saved data is permanent.</p></section>
      </>}
    </Sheet>
  </div>;
}
