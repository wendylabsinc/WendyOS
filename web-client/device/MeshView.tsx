import { useEffect, useState } from "react";
import { Network, Router, Share2 } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections } from "./operations";
import { failure } from "./rpc";
import { errorMessage } from "./presentation";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import "./MeshView.css";
const service = "wendy.agent.services.v2.WendyLocalMeshAdminService";
type MeshStatus = { configured?: { participate?: boolean; shareUplink?: boolean }; runtimeAvailable?: boolean; state?: string; detail?: string; authenticatedPeers?: number };
type Peer = { linkId?: string; principal?: string; deviceId?: string; meshAddress?: string; transport?: string; cost?: number };
type Route = { prefix?: string; nextHopPrincipal?: string; nextHopAddress?: string; local?: boolean; unreachable?: boolean };
type Announcement = { principal?: string; deviceId?: string; name?: string; services?: { appId?: string; serviceId?: string; type?: string; instance?: string; port?: number }[] };
type Topology = { selfMeshAddress?: string; runtimeAvailable?: boolean; status?: MeshStatus; peers?: Peer[]; routes?: Route[]; announcements?: Announcement[]; truncated?: boolean };
const stateNames: Record<string, string> = { disabled: "Off", "pending-runtime": "Waiting for runtime", active: "Active", connected: "Connected", idle: "Waiting for peers", disconnected: "Disconnected" };
function readable(value: string) { return value.replace(/[_-]/g, " ").replace(/^./, (letter) => letter.toUpperCase()); }
function carrier(value?: string) { return ({ tcp: "Network", ethernet: "Ethernet", ble: "Bluetooth", nan: "Wi-Fi Aware", wifi: "Wi-Fi" } as Record<string, string>)[value || ""] || (value ? readable(value) : "Network"); }
export default function MeshView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [data, setData] = useState<Topology>(), [error, setError] = useState(""), [revision, setRevision] = useState(0), [selected, setSelected] = useState<Peer>();
  useEffect(() => {
    let alive = true;
    client.call<Topology>("device-rpc", { service, method: "GetLocalMeshTopology", message: { includeServices: true } }).then((value) => {
      if (!alive) return; const problem = failure(value); if (problem) { setError(problem); setData(undefined); } else { setData(value); setError(""); }
    }).catch((problem) => { if (alive) { setError(errorMessage(problem)); setData(undefined); } });
    return () => { alive = false; };
  }, [client, refresh, revision]);
  const status = data?.status, configured = status?.configured, peers = data?.peers ?? [], routes = data?.routes ?? [];
  const operation = sections.find((section) => section.name === "Mesh")?.operations.find((item) => item.method === "ConfigureLocalMesh");
  const announcement = (peer: Peer) => data?.announcements?.find((item) => (peer.principal && item.principal === peer.principal) || (peer.deviceId && item.deviceId === peer.deviceId));
  const name = (peer: Peer) => announcement(peer)?.name || peer.meshAddress || peer.deviceId || "Mesh device";
  const hopName = (route: Route) => { const peer = peers.find((item) => route.nextHopPrincipal && item.principal === route.nextHopPrincipal); return route.local ? "This device" : peer ? name(peer) : route.nextHopAddress || "Unknown"; };
  if (/device must be enrolled|device identity unavailable/i.test(error)) return <section className="panel mesh-empty"><Network size={32} /><h2>Mesh needs device enrollment</h2><p>Enroll this device in Wendy Cloud to connect it to your mesh network.</p><a href="#Enrollment">Open enrollment</a></section>;
  if (!data) return <section className="panel mesh-empty"><Network size={32} /><h2>{error ? "Mesh unavailable" : "Checking mesh network"}</h2><p>{error || "Reading connections from this device."}</p></section>;
  const state = status?.state ? stateNames[status.state] || readable(status.state) : data.runtimeAvailable ? "Available" : "Unavailable";
  return <div className="mesh-view"><section className="panel mesh-overview"><header><div><Network size={25} /><h2>Mesh network</h2></div>{operation && <OperationCard client={client} operation={operation} refresh={refresh} presentation="header" triggerLabel="Settings" onApplied={() => setRevision((value) => value + 1)} />}</header><div className="mesh-stats"><div><span>Status</span><strong>{state}</strong></div><div><span>Connected peers</span><strong>{status?.authenticatedPeers ?? peers.length}</strong></div><div><span>Join the mesh</span><strong>{configured ? configured.participate ? "On" : "Off" : "Unknown"}</strong></div><div><span>Share internet</span><strong>{configured ? configured.shareUplink ? "On" : "Off" : "Unknown"}</strong></div></div>{status?.detail && <p className="mesh-status-note">{status.detail}</p>}{data.selfMeshAddress && <p className="mesh-address">This device · {data.selfMeshAddress}</p>}</section>
    <section className="mesh-peers"><header><h2>Nearby devices</h2><span>{peers.length} {peers.length === 1 ? "connection" : "connections"}</span></header>{peers.length ? <div className="mesh-peer-grid">{peers.map((peer, index) => <button className="panel mesh-peer" key={peer.linkId || `${peer.principal}-${index}`} onClick={() => setSelected(peer)}><Router size={25} /><h3>{name(peer)}</h3><span>{carrier(peer.transport)}</span>{peer.meshAddress && peer.meshAddress !== name(peer) && <small>{peer.meshAddress}</small>}<span className="mesh-peer-open">View connection</span></button>)}</div> : <div className="panel mesh-empty"><Router size={27} /><h3>No peers connected</h3><p>{configured?.participate ? "Other devices will appear here when they connect to this mesh." : "Turn on mesh participation in Settings to connect to other devices."}</p></div>}</section>
    {routes.length > 0 && <section className="panel mesh-routes"><header><Share2 size={20} /><h2>Network routes</h2></header><div className="mesh-route-scroll"><table><thead><tr><th>Destination</th><th>Via</th><th>Status</th></tr></thead><tbody>{routes.map((route, index) => <tr key={`${route.prefix}-${route.nextHopPrincipal}-${index}`}><td>{route.prefix}</td><td>{hopName(route)}</td><td>{route.unreachable ? "Unreachable" : route.local ? "Local" : "Available"}</td></tr>)}</tbody></table></div></section>}
    {data.truncated && <p className="mesh-status-note">Some connections are omitted from this device’s response.</p>}
    <Sheet open={!!selected} title={selected ? name(selected) : "Mesh connection"} description="Connection details reported by this device." onClose={() => setSelected(undefined)}>{selected && <><dl className="mesh-connection-details"><dt>Connection</dt><dd>{carrier(selected.transport)}</dd>{selected.meshAddress && <><dt>Mesh address</dt><dd>{selected.meshAddress}</dd></>}{selected.deviceId && <><dt>Device ID</dt><dd>{selected.deviceId}</dd></>}{selected.cost != null && <><dt>Route cost</dt><dd>{selected.cost}</dd></>}</dl>{(announcement(selected)?.services?.length ?? 0) > 0 && <section className="mesh-services"><h3>Available services</h3>{announcement(selected)?.services?.map((item, index) => <div key={`${item.serviceId}-${index}`}><strong>{item.instance || item.serviceId || item.appId || "Service"}</strong><span>{[item.type, item.port ? `Port ${item.port}` : ""].filter(Boolean).join(" · ")}</span></div>)}</section>}</>}</Sheet>
  </div>;
}
