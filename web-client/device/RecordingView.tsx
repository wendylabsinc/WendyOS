import { useEffect, useRef, useState } from "react";
import { Circle, Download, FileVideo, FolderOpen, Layers, Radio } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections, type Operation } from "./operations";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import { failure, streamID, type StreamEvent } from "./rpc";
import { useResource, ResourceError } from "./Resource";
import { bytes, errorMessage } from "./presentation";
import "./RecordingView.css";

type Episode = { id: string; name?: string; state: string; startedUnixNanos?: string; sizeBytes?: string; durationNanos?: string; campaignName?: string; sources?: string[] };
type Campaign = { name: string; state: string; deployedUnixNanos?: string; fleet?: string; warnings?: string[]; planJson?: string };
type Source = { id: string; name?: string; kind: string; healthy?: boolean; unusableReason?: string };
type Manifest = { files?: { path: string; size: number }[]; [key: string]: unknown };
const service = "wendy.agent.services.v2.DataService";
const operations = sections.find((section) => section.name === "Recordings")!.operations;
function operation(method: string, initial?: Record<string, unknown>, title?: string): Operation { return { ...operations.find((op) => op.method === method)!, ...(initial ? { initial } : {}), ...(title ? { title } : {}) }; }
function date(value?: string) { return value && Number(value) > 0 ? new Date(Number(value) / 1e6).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }) : "Date unavailable"; }
function decodeJSON(value?: string): Manifest { return value ? JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(value), (char) => char.charCodeAt(0)))) as Manifest : {}; }

export default function RecordingView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [revision, setRevision] = useState(0), [selected, setSelected] = useState<Episode>(), [campaign, setCampaign] = useState<Campaign>();
  const [showSources, setShowSources] = useState(false);
  const episodes = useResource<{ episodes?: Episode[]; inProgress?: Episode[]; reportsInProgress?: boolean }>(client, service, "Episodes", refresh + revision);
  const status = useResource<{ active?: Episode; inProgress?: Episode[] }>(client, service, "Status", refresh + revision);
  const campaigns = useResource<{ campaigns?: Campaign[] }>(client, service, "Campaigns", refresh + revision);
  const sources = useResource<{ sources?: Source[] }>(client, service, "Sources", refresh + revision);
  const current = episodes.data?.reportsInProgress ? episodes.data.inProgress || [] : status.data?.inProgress?.length ? status.data.inProgress : status.data?.active ? [status.data.active] : [];
  const items = [...new Map([...current, ...(episodes.data?.episodes || [])].map((episode) => [episode.id, episode])).values()].sort((a, b) => Number(b.startedUnixNanos || 0) - Number(a.startedUnixNanos || 0));
  const savedBytes = (episodes.data?.episodes || []).reduce((sum, item) => sum + Number(item.sizeBytes || 0), 0);
  const updated = () => setRevision((value) => value + 1);
  const control = (method: string, label: string, initial?: Record<string, unknown>) => <OperationCard client={client} operation={operation(method, initial)} refresh={refresh} presentation="header" triggerLabel={label} onApplied={updated} />;
  return <div className="recording-view">
    <div className="recording-toolbar"><div className="recording-live-state"><Radio size={20} /><span>{current.length ? `${current.length} recording${current.length === 1 ? "" : "s"} in progress` : status.loading ? "Checking recording status…" : status.error ? "Recording status unavailable" : "Ready to record"}</span></div><div className="recording-actions"><button className="quiet" onClick={() => setShowSources(true)} aria-haspopup="dialog">Sources{sources.data ? ` · ${sources.data.sources?.length || 0}` : ""}</button>{control("Start", "New recording")}{current.length > 0 && control("Stop", "Stop recording")}</div></div>
    <ResourceError error={status.error} /><ResourceError error={episodes.error} />
    <section><div className="recording-section-heading"><div><h2>Episodes</h2><p>{episodes.loading ? "Loading episodes…" : `${episodes.data?.episodes?.length || 0} saved · ${bytes(savedBytes)}`}</p></div></div>
      {!items.length && !episodes.loading && !episodes.error ? <div className="panel recording-empty"><FileVideo size={30} /><h3>No recordings yet</h3><p>Start a recording to capture data from this device.</p></div> : <div className="recording-episode-grid">{items.map((item) => <article className="panel recording-episode" key={item.id}><div className="recording-episode-top"><FileVideo size={22} /><span className="recording-state" data-active={item.state === "recording"}><Circle size={7} fill="currentColor" />{item.state}</span></div><h3>{item.name || "Untitled recording"}</h3><time>{date(item.startedUnixNanos)}</time><div className="recording-episode-meta"><strong>{bytes(item.sizeBytes || 0)}</strong>{Number(item.durationNanos) > 0 && <span>{Math.round(Number(item.durationNanos) / 1e9)} seconds</span>}</div>{item.campaignName && <p className="recording-campaign-name">{item.campaignName}</p>}<div className="recording-actions"><button className="quiet" onClick={() => setSelected(item)} aria-haspopup="dialog"><FolderOpen size={14} />Files & details</button>{!current.some((entry) => entry.id === item.id) && control("DeleteEpisode", "Delete", { episode: item.id })}</div></article>)}</div>}
    </section>
    <section><div className="recording-section-heading"><div><h2>Campaigns</h2><p>Recording rules deployed to this device.</p></div>{control("CampaignDeploy", "Deploy campaign")}</div><ResourceError error={campaigns.error} />
      {campaigns.loading ? <p className="subtle">Loading campaigns…</p> : !campaigns.data?.campaigns?.length && !campaigns.error ? <div className="panel recording-empty"><Layers size={28} /><h3>No campaigns deployed</h3><p>Deploy a campaign to start recordings from event triggers.</p></div> : <div className="recording-campaign-list">{campaigns.data?.campaigns?.map((item) => <article className="panel recording-campaign" key={item.name}><div className="recording-campaign-heading"><Layers size={22} /><div><h3>{item.name}</h3><p>{item.state} · Deployed {date(item.deployedUnixNanos)}</p></div></div>{item.warnings?.map((warning) => <p className="inline-error" key={warning}>{warning}</p>)}<div className="recording-actions"><button className="quiet" onClick={() => setCampaign(item)} aria-haspopup="dialog">Details</button>{control("CampaignTrigger", "Record now", { name: item.name })}{item.state === "armed" ? control("CampaignDisarm", "Disarm", { name: item.name }) : control("CampaignArm", "Arm", { name: item.name })}{control("CampaignRemove", "Remove", { name: item.name })}</div></article>)}</div>}
    </section>
    <Sheet open={showSources} title="Recording sources" description="Choose a source to start a recording." onClose={() => setShowSources(false)}><ResourceError error={sources.error} />{sources.loading ? <p className="subtle">Loading sources…</p> : <div className="recording-source-list">{sources.data?.sources?.map((source) => <div key={source.id}><div><h3>{source.name || source.id}</h3><p>{source.kind} · {source.healthy ? "Available" : source.unusableReason || "Unavailable"}</p></div>{source.healthy && control("Start", "Record", { sources: [source.id], name: source.name || source.id })}</div>)}</div>}</Sheet>
    <Sheet open={Boolean(selected)} title={selected?.name || "Recording files"} description={selected ? date(selected.startedUnixNanos) : undefined} onClose={() => setSelected(undefined)}>{selected && <EpisodeFiles key={selected.id} client={client} episode={selected} />}</Sheet>
    <Sheet open={Boolean(campaign)} title={campaign?.name || "Campaign"} onClose={() => setCampaign(undefined)}>{campaign && <><dl className="recording-details"><div><dt>Status</dt><dd>{campaign.state}</dd></div><div><dt>Deployed</dt><dd>{date(campaign.deployedUnixNanos)}</dd></div>{campaign.fleet && <div><dt>Fleet</dt><dd>{campaign.fleet}</dd></div>}</dl><CampaignPlan campaign={campaign} /></>}</Sheet>
  </div>;
}
function CampaignPlan({ campaign }: { campaign: Campaign }) {
  let plan: Manifest | undefined;
  try { plan = decodeJSON(campaign.planJson); } catch { /* Invalid device JSON is a display error. */ }
  return plan ? <details className="recording-manifest"><summary>Campaign configuration</summary><pre>{JSON.stringify(plan, null, 2)}</pre></details> : <p className="subtle">Campaign configuration could not be read.</p>;
}
const maxDownloadBytes = 128 * 1024 * 1024;
type DownloadChunk = { data?: string; offset?: string; size?: string; sha256?: string; eof?: boolean };
function downloadProblem(problem: string) {
  if (/unauthenticated|permission denied|permissiondenied|unauthorized|401|403/i.test(problem)) return "Your session cannot download this file. Reconnect and sign in, then try again.";
  if (/connection|websocket|transport|unavailable|deadline/i.test(problem)) return "The connection to the device was interrupted. Reconnect, then retry the download.";
  return "The file could not be downloaded. Try again.";
}
function RecordingFile({ client, episode, file }: { client: WendyClient; episode: string; file: { path: string; size: number } }) {
  const [state, setState] = useState<{ busy?: boolean; received?: number; verifying?: boolean; error?: string; url?: string }>({});
  const cancel = useRef<(() => void) | undefined>(undefined), fileURL = useRef("");
  const filename = file.path.split("/").pop() || "recording";
  useEffect(() => () => {
    cancel.current?.();
    if (fileURL.current) URL.revokeObjectURL(fileURL.current);
  }, [client, episode, file.path]);
  function start() {
    cancel.current?.();
    if (fileURL.current) URL.revokeObjectURL(fileURL.current);
    fileURL.current = "";
    const stream = streamID();
    let alive = true, completing = false, size = 0, expected = 0, eof = false, sha256 = "";
    let parts: Uint8Array<ArrayBuffer>[] = [];
    let off = () => {};
    const stop = () => {
      alive = false; off(); parts = [];
      void client.call("device-stream-stop", { stream }).catch(() => {});
    };
    cancel.current = stop;
    setState({ busy: true, received: 0 });
    function fail(problem: unknown) {
      if (!alive) return;
      setState({ error: errorMessage(problem) }); stop();
    }
    async function finish() {
      if (!eof || size !== expected) throw new Error("The device ended the download before the complete file arrived.");
      setState({ busy: true, received: size, verifying: true });
      const contents = new Uint8Array(size); let offset = 0;
      for (const part of parts) { contents.set(part, offset); offset += part.length; }
      parts = [];
      if (sha256) {
        const digest = await client.call<string>("device-sha256", { bytes: contents });
        if (digest !== sha256) throw new Error("The downloaded file failed its checksum check.");
      }
      if (!alive) return;
      const url = URL.createObjectURL(new Blob([contents])); fileURL.current = url;
      const link = document.createElement("a"); link.href = url; link.download = filename;
      document.body.appendChild(link); link.click(); link.remove();
      setState({ url }); stop();
    }
    off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (!alive || completing || event !== "device-stream" || item.stream !== stream) return;
      if (item.error) { fail(item.error); return; }
      try {
        if (item.message) {
          const problem = failure(item.message); if (problem) throw new Error(problem);
          const chunk = item.message as DownloadChunk;
          const contents = Uint8Array.from(atob(chunk.data || ""), (char) => char.charCodeAt(0));
          if (size + contents.length > maxDownloadBytes || Number(chunk.size || 0) > maxDownloadBytes) throw new Error("Browser downloads support files up to 134 MB. Use the CLI for this file.");
          if (eof || (chunk.offset !== undefined && Number(chunk.offset) !== size)) throw new Error("The file arrived out of order. Please retry the download.");
          parts.push(contents); size += contents.length; expected = Number(chunk.size || 0);
          sha256 = chunk.sha256 || sha256; eof = Boolean(chunk.eof);
          setState({ busy: true, received: size });
        }
        if (item.done) { completing = true; off(); void finish().catch(fail); }
      } catch (problem) { fail(problem); }
    });
    client.call("device-rpc", { service, method: "Download", message: { episode, path: file.path, offset: "0" }, stream }).then((result) => {
      if (!alive) { void client.call("device-stream-stop", { stream }).catch(() => {}); return; }
      const problem = failure(result); if (problem) fail(problem);
    }).catch(fail);
  }
  const tooLarge = Number(file.size) > maxDownloadBytes;
  return <div className="recording-file-row"><div className="recording-file-info"><strong>{filename}</strong><small>{bytes(file.size)}</small><details className="recording-file-path"><summary>File path</summary><code>{file.path}</code></details>{state.busy && <div className="recording-download-progress" role="status"><progress max={Number(file.size) || 1} value={Math.min(state.received || 0, Number(file.size) || 1)} aria-label={`Downloading ${filename}`} /><small>{state.verifying ? "Checking file…" : `${bytes(state.received || 0)} of ${bytes(file.size)}`}</small></div>}{state.url && <small role="status">Sent to browser downloads</small>}{tooLarge && <small>Use the CLI to download files larger than 134 MB.</small>}{state.error && <div className="recording-download-error" role="alert"><p>{downloadProblem(state.error)}</p><details><summary>Technical details</summary><pre>{state.error}</pre></details></div>}</div><div className="recording-file-actions">{state.busy ? <button className="quiet" onClick={() => { cancel.current?.(); setState({}); }}>Cancel</button> : state.url ? <a className="quiet" href={state.url} download={filename}><Download size={14} />Save again</a> : <button className="quiet" disabled={tooLarge} onClick={start}><Download size={14} />{state.error ? "Retry" : "Download"}</button>}</div></div>;
}
function EpisodeFiles({ client, episode }: { client: WendyClient; episode: Episode }) {
  const [state, setState] = useState<{ manifest?: Manifest; error?: string; checks?: string[] }>({});
  const [verify, setVerify] = useState(false);
  useEffect(() => {
    let alive = true;
    client.call<{ manifestJson?: string; verificationErrors?: string[] }>("device-rpc", { service, method: "Inspect", message: { episode: episode.id, verify } }).then((data) => { if (alive) setState({ manifest: decodeJSON(data.manifestJson), checks: verify ? data.verificationErrors || [] : undefined }); }).catch((error) => { if (alive) setState({ error: errorMessage(error) }); });
    return () => { alive = false; };
  }, [client, episode.id, verify]);
  return <><dl className="recording-details"><div><dt>Status</dt><dd>{episode.state}</dd></div><div><dt>Size</dt><dd>{bytes(episode.sizeBytes || 0)}</dd></div><div><dt>Episode ID</dt><dd>{episode.id}</dd></div></dl><ResourceError error={state.error} />{!state.manifest && !state.error && <p className="subtle">Loading files…</p>}{state.manifest && <><div className="recording-file-heading"><h3>Files</h3><button className="quiet" onClick={() => setVerify(true)} disabled={verify}>{verify ? state.checks ? state.checks.length ? "Checks complete" : "Verified" : "Verifying…" : "Verify files"}</button></div>{state.checks && <p className={state.checks.length ? "inline-error" : "subtle"}>{state.checks.length ? state.checks.join(" · ") : "All file checks passed."}</p>}<div className="recording-file-list">{state.manifest.files?.map((file) => <RecordingFile key={file.path} client={client} episode={episode.id} file={file} />)}</div><details className="recording-manifest"><summary>Recording manifest</summary><pre>{JSON.stringify(state.manifest, null, 2)}</pre></details></>}</>;
}
