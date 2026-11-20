import { useEffect, useRef, useState } from "react";
import { ArrowDown, FileText, Pause, Play } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { failure, streamID, type StreamEvent } from "./rpc";
import { errorMessage } from "./presentation";
import "./SystemLogsView.css";
type KernelRecord = { timestampUs?: string; level?: number; message?: string };
type LogRecord = KernelRecord & { key: string };
const levels = ["Emergency", "Alert", "Critical", "Error", "Warning", "Notice", "Info", "Debug"];
function elapsed(value?: string) {
  const milliseconds = Number(value) / 1000;
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "Unknown";
  const seconds = Math.floor(milliseconds / 1000), hours = Math.floor(seconds / 3600);
  return `${String(hours).padStart(2, "0")}:${String(Math.floor(seconds / 60) % 60).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}.${String(Math.floor(milliseconds % 1000)).padStart(3, "0")}`;
}
export default function SystemLogsView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [records, setRecords] = useState<LogRecord[]>([]), [error, setError] = useState(""), [running, setRunning] = useState(true), [ended, setEnded] = useState(false), [revision, setRevision] = useState(0);
  const [query, setQuery] = useState(""), [severity, setSeverity] = useState(7), [atBottom, setAtBottom] = useState(true);
  const viewport = useRef<HTMLDivElement>(null), pinned = useRef(true);
  useEffect(() => {
    if (!running) return;
    let alive = true; const stream = streamID();
    const off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (!alive || event !== "device-stream" || item.stream !== stream) return;
      if (item.error) setError(errorMessage(item.error));
      if (item.message) {
        setError(""); setEnded(false);
        const incoming = (item.message as { records?: KernelRecord[] }).records ?? [];
        setRecords((previous) => {
          const merged = new Map(previous.map((record) => [record.key, record]));
          incoming.forEach((record) => { const key = `${record.timestampUs}:${record.level}:${record.message}`; merged.set(key, { ...record, key }); });
          return [...merged.values()].sort((a, b) => Number(a.timestampUs) - Number(b.timestampUs)).slice(-1000);
        });
      }
      if (item.done) setEnded(true);
    });
    client.call("device-rpc", { service: "wendy.agent.services.v1.WendyAgentService", method: "DumpKernelLog", message: { follow: true }, stream }).then((value) => { const problem = failure(value); if (problem) throw new Error(problem); if (!alive) void client.call("device-stream-stop", { stream }).catch(() => {}); }).catch((problem) => { if (alive) { setError(errorMessage(problem)); setEnded(true); } });
    return () => { alive = false; off(); void client.call("device-stream-stop", { stream }).catch(() => {}); };
  }, [client, refresh, running, revision]);
  useEffect(() => { if (pinned.current && viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight; }, [records, query, severity]);
  const shown = records.filter((record) => (record.level == null || record.level <= severity) && (record.message || "").toLowerCase().includes(query.toLowerCase()));
  function toggle() { setError(""); setEnded(false); if (ended || error) { setRunning(true); setRevision((value) => value + 1); } else setRunning(!running); }
  const following = running && !ended && !error;
  return <section className="panel system-log-view"><header className="system-log-toolbar"><div><h2>Kernel messages</h2><p>{error ? "Connection interrupted" : !running ? "Paused" : ended ? "Stream ended" : "Following new messages"} · Up to 1,000 recent messages</p></div><div className="system-log-controls"><input aria-label="Search system logs" placeholder="Search messages…" value={query} onChange={(event) => setQuery(event.target.value)} /><select aria-label="System log severity" value={severity} onChange={(event) => setSeverity(Number(event.target.value))}><option value={7}>All levels</option><option value={6}>Info and above</option><option value={4}>Warnings and errors</option><option value={3}>Errors only</option></select><button className="quiet" onClick={toggle}>{following ? <Pause size={15} /> : <Play size={15} />}{following ? "Pause" : "Resume"}</button></div></header>
    {error && <p className="inline-error" role="alert">{error}</p>}
    <div className="system-log-columns" aria-hidden="true"><span>Since boot</span><span>Level</span><span>Message</span></div>
    <div className="system-log-output" ref={viewport} role="log" aria-label="Kernel messages" aria-live="off" tabIndex={0} onScroll={() => { if (!viewport.current) return; const bottom = viewport.current.scrollHeight - viewport.current.scrollTop - viewport.current.clientHeight < 40; pinned.current = bottom; setAtBottom(bottom); }}>
      {shown.map((record) => <div className="system-log-line" key={record.key} data-severity={record.level ?? 6}><time title="Elapsed time since device boot">{elapsed(record.timestampUs)}</time><span className="system-log-level">{record.level !== undefined ? levels[record.level] || "Unknown" : "Unknown"}</span><span className="system-log-message">{record.message}</span></div>)}
      {!shown.length && <div className="system-log-empty"><FileText size={25} /><h3>{records.length ? "No matching messages" : error ? "Logs unavailable" : "Waiting for system messages"}</h3><p>{records.length ? "Try another search or severity level." : "Messages from the device kernel appear here."}</p></div>}
    </div><footer className="system-log-footer"><span>{shown.length} {shown.length === 1 ? "message" : "messages"}</span>{!atBottom && <button className="quiet" onClick={() => { pinned.current = true; setAtBottom(true); if (viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight; }}><ArrowDown size={14} />Jump to latest</button>}</footer>
  </section>;
}
