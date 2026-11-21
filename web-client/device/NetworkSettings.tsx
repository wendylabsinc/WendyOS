import { useState } from "react";
import { Cable, Check, Copy, Cpu, Network, Wifi, WifiOff } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections } from "./operations";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import { interfaceKind, unavailableFeature } from "./presentation";
import { useResource, ResourceError } from "./Resource";
import "./NetworkSettings.css";

type Interface = { name: string; ipAddresses: string[] };
type WiFiStatus = { connected: boolean; ssid?: string };
function CopyAddress({ address }: { address: string }) {
  const [copied, setCopied] = useState(false), [error, setError] = useState("");
  async function copy() {
    setCopied(false); setError("");
    try {
      if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(address);
      else {
        // The panel may use HTTP on a LAN, where Clipboard API is unavailable.
        const focus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
        const input = document.createElement("textarea"); input.value = address;
        input.style.cssText = "position:fixed;opacity:0;pointer-events:none";
        document.body.appendChild(input); input.select();
        try { if (!document.execCommand("copy")) throw new Error("Copy unavailable"); }
        finally { input.remove(); focus?.focus(); }
      }
      setCopied(true);
    } catch { setError("Select the address to copy it."); }
  }
  return <div className="address-entry"><span className="address-family">{address.includes(":") ? "IPv6" : "IPv4"}</span><code>{address}</code><button type="button" className="quiet" aria-label={`Copy ${address}`} onClick={() => void copy()}>{copied ? <Check size={14} /> : <Copy size={14} />}{copied ? "Copied" : "Copy"}</button>{error && <span className="address-copy-error" role="status">{error}</span>}</div>;
}
function Connection({ item }: { item: Interface }) {
  const kind = interfaceKind(item.name), InterfaceIcon = kind === "Wi-Fi" ? Wifi : Cable;
  return <article className="panel lan-card"><header className="lan-card-heading"><div><h2>{kind}</h2><span className="interface-name">{item.name}</span></div><span className="connection-state"><i />Connected</span></header>
    <div className="lan-diagram" aria-label={`This device is connected through ${item.name}`}>
      <div className="lan-node"><div className="lan-node-icon"><Cpu size={26} /></div><span>This device</span></div><span className="lan-link" aria-hidden="true" />
      <div className="lan-node"><div className="lan-node-icon connected"><InterfaceIcon size={26} /></div><span>{kind}</span></div><span className="lan-link" aria-hidden="true" />
      <div className="lan-node"><div className="lan-node-icon"><Network size={26} /></div><span>{kind === "Tunnel" ? "Tunnel network" : "Local network"}</span></div>
    </div>
    <div className="interface-addresses"><h3>Device addresses</h3>{item.ipAddresses.map((address) => <CopyAddress key={address} address={address} />)}</div>
  </article>;
}
export default function NetworkSettings({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [editing, setEditing] = useState(false), [revision, setRevision] = useState(0);
  const network = useResource<{ networkInterfaces?: Interface[] }>(client, "wendy.agent.services.v1.WendyAgentService", "GetAgentVersion", refresh + revision);
  const wifi = useResource<WiFiStatus>(client, "wendy.agent.services.v2.WendyWiFiService", "GetWiFiStatus", refresh + revision);
  const absent = unavailableFeature(wifi.error, "wifi"), interfaces = network.data?.networkInterfaces ?? [];
  const operations = sections.find((section) => section.name === "Network")!.operations;
  const action = (method: string, triggerLabel?: string) => <OperationCard client={client} operation={operations.find((operation) => operation.method === method)!} refresh={refresh} presentation="header" triggerLabel={triggerLabel} onApplied={() => setRevision((value) => value + 1)} />;
  const advanced = operations.filter((operation) => !["GetWiFiStatus", "ListKnownWiFiNetworks", "ListWiFiNetworks", "ConnectToWiFi"].includes(operation.method));
  return <div className="network-view"><ResourceError error={network.error} />
    {interfaces.length > 0 && <div className="network-card-grid">{interfaces.map((item) => <Connection key={item.name} item={item} />)}</div>}
    {!interfaces.length && <section className="panel network-empty"><Network size={32} /><h2>{network.loading ? "Loading connections…" : "No active LAN addresses reported"}</h2>{!network.loading && <p className="subtle">The device has not reported a usable network interface.</p>}</section>}
    {absent ? <div className="wifi-unavailable"><WifiOff size={20} /><div><h2>Wi-Fi unavailable</h2><p>No Wi-Fi adapter is installed on this device.</p></div></div> : <section className="panel wireless-card"><header className="settings-heading settings-heading-with-action"><div><h2>Wi-Fi</h2><p className="subtle">Wireless connection and saved networks.</p></div>{wifi.data && <button className="quiet" aria-haspopup="dialog" onClick={() => setEditing(true)}>Edit</button>}</header><ResourceError error={wifi.error} />
      {wifi.loading ? <p className="settings-body subtle">Checking Wi-Fi…</p> : wifi.data && <div className="wireless-summary"><div className={`wireless-icon ${wifi.data.connected ? "connected" : "disconnected"}`}>{wifi.data.connected ? <Wifi size={32} /> : <WifiOff size={32} />}</div><div><strong>{wifi.data.ssid || "Not connected to a network"}</strong><span className={`connection-state ${wifi.data.connected ? "" : "disconnected"}`}><i />{wifi.data.connected ? "Connected" : "Disconnected"}</span></div></div>}
    </section>}
    <Sheet open={editing && Boolean(wifi.data)} title="Wi-Fi settings" description="Connect to a network or manage saved connections." onClose={() => setEditing(false)}>
      <div className="wifi-sheet-section"><div className="wifi-sheet-heading"><h3>Networks</h3><div className="wifi-sheet-actions">{action("ListWiFiNetworks", "Find networks")}{action("ConnectToWiFi", "Join network")}</div></div></div>
      {editing && wifi.data && <OperationCard client={client} operation={operations.find((operation) => operation.method === "ListKnownWiFiNetworks")!} refresh={refresh + revision} />}
      <details className="wifi-advanced"><summary>Advanced settings</summary>{editing && wifi.data && advanced.map((operation) => <OperationCard key={operation.method} client={client} operation={operation} refresh={refresh + revision} compact onApplied={() => setRevision((value) => value + 1)} />)}</details>
    </Sheet>
  </div>;
}
