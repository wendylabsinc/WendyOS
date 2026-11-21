import { useCallback, useEffect, useRef, useState } from "react";
import type { WendyClient } from "@/lib/client";
import type { Operation } from "./operations";
import { failure, rpc, streamID, type Field, type StreamEvent } from "./rpc";
import Fields from "./Fields";
import Result from "./Result";
import Media from "./Media";
import Sheet from "./Sheet";
import { operationFields, operationInput } from "./operation-policy";
import { errorMessage } from "./presentation";

export default function OperationCard({ client, operation, refresh, compact = false, presentation, triggerLabel, onApplied }: { client: WendyClient; operation: Operation; refresh: number; compact?: boolean; presentation?: "header"; triggerLabel?: string; onApplied?: () => void }) {
  const destructive = !operation.read && /^(Delete|Remove|Forget|Unprovision|PruneCache)/.test(operation.method);
  const [fields, setFields] = useState<Field[]>([]);
  const [values, setValues] = useState<Record<string, unknown>>(operation.initial ?? {});
  const [editing, setEditing] = useState(false), [review, setReview] = useState(false);
  const [configuring, setConfiguring] = useState(false);
  const [busy, setBusy] = useState(false), [error, setError] = useState("");
  const [result, setResult] = useState<unknown>(), [messages, setMessages] = useState<unknown[]>([]);
  const [stream, setStream] = useState(""), [updated, setUpdated] = useState("");
  const active = useRef("");
  const runGeneration = useRef(0), configurationGeneration = useRef(0);
  const invalidatePendingWork = useCallback(() => { runGeneration.current++; configurationGeneration.current++; }, []);
  const mediaReady = useRef<((error?: string) => void) | undefined>(undefined);
  const onMediaReady = useCallback((error?: string) => { mediaReady.current?.(error); mediaReady.current = undefined; }, []);
  const download = useRef({ parts: [] as Uint8Array<ArrayBuffer>[], size: 0, expected: 0, sha256: "", eof: false, name: "download" });
  const [file, setFile] = useState<{ url: string; name: string }>();
  const fileURL = useRef("");
  async function finishDownload() {
    const generation = runGeneration.current;
    const item = download.current;
    if (operation.method === "Download" && (!item.eof || item.size !== item.expected)) throw new Error("The device ended the download before the complete file arrived.");
    const bytes = new Uint8Array(item.size); let offset = 0;
    for (const part of item.parts) { bytes.set(part, offset); offset += part.length; }
    if (item.sha256) {
      const digest = await client.call<string>("device-sha256", { bytes });
      if (digest !== item.sha256) throw new Error("The downloaded file failed its checksum check.");
    }
    if (!alive.current || generation !== runGeneration.current) return;
    const url = URL.createObjectURL(new Blob([bytes])); fileURL.current = url;
    setFile({ url, name: item.name }); item.parts = [];
  }
  const alive = useRef(true);
  const media = operation.method === "StreamVideo" || operation.method === "StreamAudio";
  async function stop(invalidate = true) {
    if (invalidate) runGeneration.current++;
    mediaReady.current?.("Stream closed"); mediaReady.current = undefined;
    const id = active.current; active.current = ""; setStream(""); setBusy(false);
    if (id) await client.call("device-stream-stop", { stream: id }).catch(() => {});
  }
  async function run(input: Record<string, unknown>) {
    const generation = ++runGeneration.current;
    await stop(false);
    if (generation !== runGeneration.current) return;
    if (fileURL.current) URL.revokeObjectURL(fileURL.current); setFile(undefined);
    download.current = { parts: [], size: 0, expected: 0, sha256: "", eof: false, name: operation.method === "DownloadBag" ? "recording.tar" : "download" };
    if (!alive.current) return;
    setError(""); setBusy(true); setResult(undefined); setMessages([]); setReview(false);
    const id = operation.stream ? streamID() : "";
    active.current = id; setStream(id);
    try {
      // Allow the media component to subscribe before the first frame arrives.
      if (media) await new Promise<void>((resolve, reject) => { mediaReady.current = (error) => error ? reject(new Error(error)) : resolve(); });
      if (generation !== runGeneration.current) return;
      const value = await rpc(client, operation, operationInput(operation, input), id);
      if (!alive.current || generation !== runGeneration.current) { if (id) void client.call("device-stream-stop", { stream: id }); return; }
      const problem = failure(value);
      if (problem) throw new Error(problem);
      if (!operation.stream) { setResult(value); setBusy(false); setUpdated(new Date().toLocaleTimeString()); }
      if (operation.automatic) setEditing(false);
      if (!operation.read && !operation.stream) onApplied?.();
      // Clear entered secrets after submission.
      setValues(operation.initial ?? {});
    } catch (e) {
      if (alive.current && generation === runGeneration.current) { setError(errorMessage(e)); setBusy(false); setStream(""); }
    }
  }
  useEffect(() => {
    alive.current = true;
    const off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (event !== "device-stream" || item.stream !== active.current) return;
      if (item.error) setError(item.error);
      if (item.message) {
        const problem = failure(item.message); if (problem) setError(problem);
        if (operation.download) {
          const chunk = item.message as { data?: string; offset?: string; size?: string; sha256?: string; eof?: boolean; path?: string };
          const saved = download.current;
          const bytes = Uint8Array.from(atob(chunk.data ?? ""), (c) => c.charCodeAt(0));
          if (saved.size + bytes.length > 128 * 1024 * 1024 || (chunk.offset !== undefined && Number(chunk.offset) !== saved.size)) {
            setError("The download exceeded 134 MB or arrived out of order. Use the CLI for larger files."); void stop(); return;
          }
          saved.parts.push(bytes); saved.size += bytes.length; saved.expected = Number(chunk.size || 0);
          saved.sha256 = chunk.sha256 || saved.sha256; saved.eof = Boolean(chunk.eof);
          if (chunk.path) saved.name = chunk.path.split("/").pop() || "download";
          setResult({ receivedBytes: saved.size });
        } else if (!media) setMessages((previous) => [...previous, item.message].slice(-50));
        setUpdated(new Date().toLocaleTimeString());
      }
      if (item.done) { setBusy(false); setStream(""); if (!operation.read) onApplied?.(); if (operation.download && !item.error) void finishDownload().catch((e) => setError(errorMessage(e))); }
    });
    if (operation.automatic) void client.call<Field[]>("device-schema", { service: operation.service, method: operation.method }).then((schema) => { if (alive.current) setFields(operationFields(operation, schema)); }).catch(() => {});
    const timer = operation.automatic ? setTimeout(() => void run(operation.initial ?? {}), 0) : undefined;
    return () => {
      alive.current = false; invalidatePendingWork(); off(); clearTimeout(timer);
      if (fileURL.current) URL.revokeObjectURL(fileURL.current);
      mediaReady.current?.("Stream closed"); mediaReady.current = undefined;
      const id = active.current; active.current = "";
      if (id) void client.call("device-stream-stop", { stream: id }).catch(() => {});
    };
    // Each card is keyed by its operation; refresh is an explicit user action.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, refresh]);
  async function configure() {
    const generation = ++configurationGeneration.current;
    setError(""); setReview(false); setResult(undefined); setMessages([]);
    setEditing(true); setConfiguring(true);
    try {
      const schema = await client.call<Field[]>("device-schema", { service: operation.service, method: operation.method });
      if (!alive.current || generation !== configurationGeneration.current) return;
      setFields(operationFields(operation, schema));
      if (operation.read && schema.length === 0) await run(values);
    } catch (e) { if (alive.current && generation === configurationGeneration.current) setError(errorMessage(e)); }
    finally { if (alive.current && generation === configurationGeneration.current) setConfiguring(false); }
  }
  function closeSheet() {
    configurationGeneration.current++;
    setEditing(false); setReview(false); setValues(operation.initial ?? {});
    if (!operation.automatic) {
      void stop();
      download.current.parts = [];
      if (fileURL.current) URL.revokeObjectURL(fileURL.current);
      fileURL.current = ""; setFile(undefined);
    }
  }
  const output = <>
    {stream && media && <Media client={client} stream={stream} audio={operation.method === "StreamAudio"} onReady={onMediaReady} />}
    {result !== undefined && <div className="device-output"><Result value={result} /></div>}
    {messages.length > 0 && <div className="device-output device-stream-output">{messages.map((message, i) => <Result key={i} value={message} />)}</div>}
    {busy && <p className="subtle" role="status">{operation.stream ? "Receiving from device…" : "Waiting for device…"}</p>}
    {file && <a className="quiet" href={file.url} download={file.name}>Save {file.name}</a>}
    {updated && <span className="sr-only">Updated {updated}</span>}
  </>;
  const buttons = <div className="operation-buttons">
    {operation.automatic && fields.length > 0 && <button className="quiet" onClick={() => void configure()} aria-haspopup="dialog">Options</button>}
    <button className={destructive ? "quiet danger" : "quiet"} disabled={busy && !operation.stream} aria-haspopup={operation.automatic ? undefined : "dialog"} onClick={() => busy && operation.automatic ? void stop() : operation.automatic ? void run(operation.initial ?? {}) : void configure()}>{busy && operation.automatic ? operation.stream ? "Stop" : "Working…" : operation.automatic ? "Refresh" : triggerLabel || (operation.read ? "Open" : "Edit")}</button>
  </div>;
  const finished = !operation.read && !operation.stream && result !== undefined;
  return <>
    {presentation === "header" ? buttons : <section className={compact ? "device-operation operation-row" : "panel device-operation"}>
      <div className="panel-heading"><h2>{operation.title}</h2>{buttons}</div>
      {operation.automatic && error && <div role="alert" className="inline-error">{error}</div>}
      {operation.automatic && output}
    </section>}
    <Sheet open={editing} title={operation.title} description={operation.help} onClose={closeSheet}>
      {error && <div role="alert" className="inline-error">{error}</div>}
      {configuring ? <p className="subtle" role="status">Loading settings…</p> : !finished && <form onSubmit={(event) => { event.preventDefault(); if (operation.read || review) void run(values); else setReview(true); }}>
        <fieldset disabled={busy} className="sheet-form-fields">
          {review ? <div className="device-review"><p>Apply <strong>{operation.title.toLowerCase()}</strong> to this device?</p><Result value={values} /><p className="subtle">{operation.help || "This changes the device's configuration or running applications."}</p></div> : <Fields fields={fields} value={values} onChange={setValues} />}
        </fieldset>
        <div className="actions"><button className={destructive ? "primary danger" : "primary"} disabled={busy} type="submit">{busy ? "Working…" : review ? destructive ? operation.title : "Confirm and apply" : operation.read ? operation.title : "Review changes"}</button><button className="quiet" type="button" onClick={closeSheet}>Cancel</button></div>
      </form>}
      {finished && <p className="sheet-success" role="status">Changes applied.</p>}
      {!operation.automatic && output}
      {stream && <button className="quiet" type="button" onClick={() => void stop()}>Stop</button>}
      {finished && <div className="actions"><button className="primary" type="button" onClick={closeSheet}>Done</button></div>}
    </Sheet>
  </>;
}
