import { useEffect, useState } from "react";
import { Box, Database, Network, Radio, Settings2 } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections, type Operation } from "./operations";
import { bytes, errorMessage } from "./presentation";
import { failure, streamID, type StreamEvent } from "./rpc";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import "./ROSView.css";
const service = "wendy.agent.services.v2.ROS2Service";
const operations = sections.find((section) => section.name === "ROS 2")!.operations;
type Node = { name?: string; namespace?: string; rmw?: string };
type Topic = { name?: string; types?: string[]; publisherCount?: number; subscriberCount?: number; rmw?: string };
type Service = { name?: string; types?: string[]; rmw?: string };
type Bag = { name?: string; sizeBytes?: string; createdUnix?: string; durationSeconds?: number };
type State<T> = { data?: T; error?: string };
function useROS<T>(client: WendyClient, method: string, refresh: number, domain: string) {
  const [state, setState] = useState<State<T>>({});
  useEffect(() => {
    let alive = true;
    const message = { ...(domain !== "" ? { domainId: Number(domain) } : {}), ...(method === "ListTopics" ? { includeCounts: true } : {}) };
    client.call<T>("device-rpc", { service, method, message: method === "ListBags" ? {} : message }).then((value) => { if (alive) { const error = failure(value); setState(error ? { error } : { data: value }); } }).catch((error) => { if (alive) setState({ error: errorMessage(error) }); });
    return () => { alive = false; };
  }, [client, method, refresh, domain]);
  return state;
}
function middleware(value: string) { return ({ rmw_fastrtps_cpp: "Fast DDS", rmw_fastrtps_dynamic_cpp: "Fast DDS", rmw_cyclonedds_cpp: "Cyclone DDS", rmw_zenoh_cpp: "Zenoh", rmw_connextdds: "Connext DDS" } as Record<string, string>)[value] || value; }
function nodeName(node: Node) { return node.name?.startsWith("/") ? node.name : `${node.namespace || "/"}/${node.name || ""}`.replace(/\/+/g, "/"); }
export default function ROSView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [domainInput, setDomainInput] = useState(""), [domain, setDomain] = useState("");
  return <ROSContent key={domain} client={client} refresh={refresh} domain={domain} domainInput={domainInput} onDomainInput={setDomainInput} onDomain={() => setDomain(domainInput)} />;
}
function ROSContent({ client, refresh, domain, domainInput, onDomainInput, onDomain }: { client: WendyClient; refresh: number; domain: string; domainInput: string; onDomainInput: (value: string) => void; onDomain: () => void }) {
  const nodes = useROS<{ nodes?: Node[] }>(client, "ListNodes", refresh, domain), topics = useROS<{ topics?: Topic[] }>(client, "ListTopics", refresh, domain), services = useROS<{ services?: Service[] }>(client, "ListServices", refresh, domain), bags = useROS<{ bags?: Bag[] }>(client, "ListBags", refresh, domain);
  const [tab, setTab] = useState("Topics"), [query, setQuery] = useState(""), [advanced, setAdvanced] = useState(false), [selected, setSelected] = useState<{ operation: Operation; title: string }>();
  const domainValues = domain !== "" ? { domainId: Number(domain) } : {};
  function open(method: string, initial: Record<string, unknown>, title: string) { const operation = operations.find((item) => item.method === method); if (operation) setSelected({ title, operation: { ...operation, initial: { ...operation.initial, ...(method === "DownloadBag" ? {} : domainValues), ...initial }, automatic: Boolean(operation.read) } }); }
  const matches = (value: string) => value.toLowerCase().includes(query.toLowerCase());
  const currentError = tab === "Nodes" ? nodes.error : tab === "Topics" ? topics.error : tab === "Services" ? services.error : bags.error;
  const currentData = tab === "Nodes" ? nodes.data : tab === "Topics" ? topics.data : tab === "Services" ? services.data : bags.data;
  const items = tab === "Nodes" ? (nodes.data?.nodes ?? []).filter((item) => matches(nodeName(item))) : tab === "Topics" ? (topics.data?.topics ?? []).filter((item) => matches(`${item.name} ${item.types?.join(" ")}`)) : tab === "Services" ? (services.data?.services ?? []).filter((item) => matches(`${item.name} ${item.types?.join(" ")}`)) : (bags.data?.bags ?? []).filter((item) => matches(item.name || ""));
  const summaries = [{ title: "Nodes", icon: Box, count: nodes.data ? (nodes.data.nodes ?? []).length : undefined }, { title: "Topics", icon: Radio, count: topics.data ? (topics.data.topics ?? []).length : undefined }, { title: "Services", icon: Network, count: services.data ? (services.data.services ?? []).length : undefined }, { title: "Recordings", icon: Database, count: bags.data ? (bags.data.bags ?? []).length : undefined }];
  return <div className="ros-view"><div className="ros-toolbar"><form onSubmit={(event) => { event.preventDefault(); onDomain(); }}><label htmlFor="ros-domain">ROS domain</label><input id="ros-domain" type="number" min={0} max={232} step={1} value={domainInput} onChange={(event) => onDomainInput(event.target.value)} placeholder="Default" /><button className="quiet" type="submit" disabled={domainInput === domain}>Apply</button></form><button className="quiet" onClick={() => setAdvanced(true)}><Settings2 size={15} />Advanced</button></div>
    <div className="ros-summaries">{summaries.map(({ title, icon: Icon, count }) => <button className="panel ros-summary" key={title} onClick={() => { setTab(title); setQuery(""); }}><Icon size={23} /><span>{title}</span><strong>{count ?? "—"}</strong></button>)}</div>
    <section className="panel ros-browser"><header><nav aria-label="ROS resources">{summaries.map(({ title }) => <button key={title} aria-pressed={tab === title} onClick={() => { setTab(title); setQuery(""); }}>{title}</button>)}</nav><input aria-label={`Search ${tab.toLowerCase()}`} value={query} onChange={(event) => setQuery(event.target.value)} placeholder={`Search ${tab.toLowerCase()}…`} /></header>
      {currentError ? <div className="ros-empty"><Radio size={27} /><h3>Unable to read {tab.toLowerCase()}</h3><p>{currentError}</p></div> : !currentData ? <div className="ros-empty"><p>Reading {tab.toLowerCase()}…</p></div> : !items.length ? <div className="ros-empty"><Radio size={27} /><h3>{query ? "No matches" : `No ${tab.toLowerCase()} found`}</h3><p>{query ? "Try another name." : tab === "Recordings" ? "Saved ROS recordings will appear here." : "Resources from running ROS applications appear in this domain."}</p></div> : items.map((raw, index) => {
        if (tab === "Nodes") { const item = raw as Node, name = nodeName(item); return <article className="ros-row" key={`${name}-${item.rmw}-${index}`}><Box size={20} /><div><h3>{name}</h3>{item.rmw && <p>{middleware(item.rmw)}</p>}</div><div className="ros-row-actions"><button className="quiet" onClick={() => open("ListParams", { node: name }, name)}>Parameters</button></div></article>; }
        if (tab === "Topics") { const item = raw as Topic; return <article className="ros-row" key={`${item.name}-${item.rmw}-${index}`}><Radio size={20} /><div><h3>{item.name}</h3><p>{item.types?.join(" · ")}</p><small>{item.publisherCount ?? "—"} publishers · {item.subscriberCount ?? "—"} subscribers{item.rmw ? ` · ${middleware(item.rmw)}` : ""}</small></div><div className="ros-row-actions"><button className="quiet" onClick={() => open("EchoTopic", { topic: item.name }, item.name || "Topic messages")}>Watch</button><button className="quiet" onClick={() => open("MonitorHz", { topic: item.name }, "Topic frequency")}>Frequency</button><button className="quiet" onClick={() => open("GetTopicInfo", { topic: item.name }, item.name || "Topic details")}>Details</button></div></article>; }
        if (tab === "Services") { const item = raw as Service; return <article className="ros-row" key={`${item.name}-${item.rmw}-${index}`}><Network size={20} /><div><h3>{item.name}</h3><p>{item.types?.join(" · ")}</p>{item.rmw && <small>{middleware(item.rmw)}</small>}</div><div className="ros-row-actions"><OperationCard client={client} refresh={refresh} presentation="header" triggerLabel="Call service" operation={{ ...operations.find((operation) => operation.method === "CallService")!, initial: { ...domainValues, service: item.name, ...(item.types?.length === 1 ? { type: item.types[0] } : {}) } }} /></div></article>; }
        const item = raw as Bag, created = Number(item.createdUnix); return <article className="ros-row" key={`${item.name}-${index}`}><Database size={20} /><div><h3>{item.name}</h3><p>{bytes(item.sizeBytes)}{item.durationSeconds ? ` · ${item.durationSeconds.toLocaleString(undefined, { maximumFractionDigits: 1 })} seconds` : ""}</p>{created > 0 && <small>{new Date(created * 1000).toLocaleString()}</small>}</div><div className="ros-row-actions"><button className="quiet" onClick={() => open("DownloadBag", { name: item.name }, item.name || "Download recording")}>Download</button></div></article>;
      })}</section>
    <Sheet open={!!selected} title={selected?.title || "ROS details"} onClose={() => setSelected(undefined)}>{selected && (selected.operation.method === "EchoTopic" || selected.operation.method === "MonitorHz" ? <TopicLive key={`${selected.operation.method}-${selected.title}`} client={client} operation={selected.operation} /> : <OperationCard key={`${selected.operation.method}-${selected.title}`} client={client} operation={selected.operation} refresh={refresh} />)}</Sheet>
    <Sheet open={advanced} title="Advanced ROS tools" description="Inspect and manage resources in the selected domain." onClose={() => setAdvanced(false)}><div className="ros-advanced">{operations.map((operation) => <OperationCard key={operation.method} client={client} refresh={refresh} operation={{ ...operation, initial: { ...operation.initial, ...(operation.method === "ListBags" || operation.method === "DownloadBag" || operation.method === "RecordBag" ? {} : domainValues) }, automatic: false }} compact />)}</div></Sheet>
  </div>;
}

function TopicLive({ client, operation }: { client: WendyClient; operation: Operation }) {
  const [messages, setMessages] = useState<string[]>([]), [sample, setSample] = useState<{ hz?: number; minDelta?: number; maxDelta?: number; window?: number }>(), [error, setError] = useState(""), [running, setRunning] = useState(true), [ended, setEnded] = useState(false), [revision, setRevision] = useState(0);
  const frequency = operation.method === "MonitorHz";
  useEffect(() => {
    if (!running) return;
    let alive = true; const stream = streamID();
    const off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (!alive || event !== "device-stream" || item.stream !== stream) return;
      if (item.error) setError(errorMessage(item.error));
      if (item.message) {
        setError(""); setEnded(false);
        if (frequency) setSample(item.message as { hz?: number; minDelta?: number; maxDelta?: number; window?: number });
        else { const yaml = (item.message as { yaml?: string }).yaml; if (yaml) setMessages((previous) => [...previous, yaml].slice(-20)); }
      }
      if (item.done) setEnded(true);
    });
    client.call("device-rpc", { service, method: operation.method, message: operation.initial ?? {}, stream }).then((value) => { const problem = failure(value); if (problem) throw new Error(problem); if (!alive) void client.call("device-stream-stop", { stream }).catch(() => {}); }).catch((problem) => { if (alive) { setError(errorMessage(problem)); setEnded(true); } });
    return () => { alive = false; off(); void client.call("device-stream-stop", { stream }).catch(() => {}); };
  }, [client, frequency, operation, revision, running]);
  const active = running && !ended && !error;
  return <div className="ros-topic-live"><header><p>{active ? "Listening for messages" : error ? "Connection interrupted" : "Paused"}</p><button className="quiet" onClick={() => { setError(""); setEnded(false); if (!active) { setRunning(true); setRevision((value) => value + 1); } else setRunning(false); }}>{active ? "Pause" : "Resume"}</button></header>{error && <p className="inline-error" role="alert">{error}</p>}{frequency ? <section className="ros-frequency"><span>Messages per second</span><strong>{sample?.hz != null ? `${sample.hz.toLocaleString(undefined, { maximumFractionDigits: 2 })} Hz` : "Waiting for samples…"}</strong>{sample && <dl>{sample.minDelta != null && <><dt>Shortest interval</dt><dd>{(sample.minDelta * 1000).toLocaleString(undefined, { maximumFractionDigits: 2 })} ms</dd></>}{sample.maxDelta != null && <><dt>Longest interval</dt><dd>{(sample.maxDelta * 1000).toLocaleString(undefined, { maximumFractionDigits: 2 })} ms</dd></>}{sample.window != null && <><dt>Sample window</dt><dd>{sample.window} messages</dd></>}</dl>}</section> : <div className="ros-topic-messages" role="log" aria-live="off" aria-label="Topic messages">{messages.length ? messages.map((message, index) => <pre key={index}>{message}</pre>) : <p>Waiting for topic messages…</p>}</div>}</div>;
}
