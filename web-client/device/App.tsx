import { useEffect, useState } from "react";
import { Bluetooth, Box, Cable, Camera, ChartLine, Cpu, Download, FileText, HardDrive, LayoutDashboard, LogOut, Menu, Network, Plug, RefreshCw, Route, ScrollText, Settings, ShieldCheck, Terminal, Video, Volume2, Workflow, X } from "lucide-react";
import { WendyClient } from "@/lib/client";
import DeviceDashboard from "@/components/device-dashboard";
import WendyTerminal from "@/components/wendy-terminal";
import { sections } from "./operations";
import StorageView from "./StorageView";
import ConnectivityView from "./ConnectivityView";
import MeshView from "./MeshView";
import SystemLogsView from "./SystemLogsView";
import ROSView from "./ROSView";
import UpdatesView from "./UpdatesView";
import RecordingView from "./RecordingView";
import HardwareView from "./HardwareView";
import Applications from "./Applications";
import SectionView from "./SectionView";
import DeviceSettings from "./DeviceSettings";
import NetworkSettings from "./NetworkSettings";
import brandLogo from "../../screencast/support/wendy-logo-cream.svg";
import "./Navigation.css";
// This static Vite entry bundles the existing logo; it has no Next image server.
// eslint-disable-next-line @next/next/no-img-element
function Brand() { return <div className="device-wordmark"><img src={brandLogo} alt="Wendy" /><span>WendyOS</span></div>; }
const icons: Record<string, typeof Cpu> = {
  Overview: LayoutDashboard, Device: Cpu, Applications: Box, Network: Cable, Mesh: Network,
  Bluetooth, Cameras: Camera, Audio: Volume2, "Sensors & pairing": Plug,
  Enrollment: ShieldCheck, Storage: HardDrive, "Updates & drivers": Download, "ROS 2": Workflow, Recordings: Video,
  "System logs": ScrollText, Logs: FileText, Metrics: ChartLine, Traces: Route, Terminal,
};
const navigationGroups = [
  { name: "Device", views: ["Overview", "Device", "Applications", "Network", "Mesh"] },
  { name: "Hardware", views: ["Bluetooth", "Cameras", "Audio", "Sensors & pairing"] },
  { name: "System", views: ["Enrollment", "Storage", "Updates & drivers", "ROS 2", "Recordings"] },
  { name: "Diagnostics", views: ["System logs", "Logs", "Metrics", "Traces", "Terminal"] },
];
const views = ["Overview", ...sections.map((section) => section.name), "Logs", "Metrics", "Traces", "Terminal"];
export default function App() {
  const [client, setClient] = useState<WendyClient>(), [name, setName] = useState("This device");
  const [view, setView] = useState(() => { try { const hash = decodeURIComponent(window.location.hash.slice(1)); const selected = hash === "Wi-Fi" ? "Network" : hash; return views.includes(selected) ? selected : "Overview"; } catch { return "Overview"; } }), [menu, setMenu] = useState(false);
  const [session, setSession] = useState<{ authenticated: boolean; open: boolean }>();
  const [logApp, setLogApp] = useState("");
  const [password, setPassword] = useState(""), [error, setError] = useState("");
  const [busy, setBusy] = useState(false), [retry, setRetry] = useState(0), [refresh, setRefresh] = useState(0);
  useEffect(() => {
    const changeView = () => {
      try {
        const hash = decodeURIComponent(window.location.hash.slice(1));
        const next = hash === "Wi-Fi" ? "Network" : hash;
        if (views.includes(next)) { setView(next); setMenu(false); }
      } catch { /* Ignore malformed links. */ }
    };
    window.addEventListener("hashchange", changeView);
    return () => window.removeEventListener("hashchange", changeView);
  }, []);
  useEffect(() => {
    let alive = true;
    fetch("/api/session").then((r) => { if (!r.ok) throw new Error("Could not read device session"); return r.json() as Promise<{ authenticated: boolean; open: boolean }>; }).then((value) => { if (alive) setSession(value); }).catch((e) => { if (alive) setError(String(e)); });
    return () => { alive = false; };
  }, []);
  useEffect(() => {
    if (!session?.authenticated) return;
    let alive = true; const connection = new WendyClient();
    // The connection result drives the UI; opening a page does not run an action.
    connection.call<Record<string, unknown>>("device-connect").then((version) => {
      if (!alive) return;
      setName(String(version.hostname || version.deviceName || location.hostname)); setClient(connection); setBusy(false);
    }).catch((e) => { if (alive) { setError(String(e)); setBusy(false); } });
    return () => { alive = false; connection.dispose(); setClient(undefined); };
  }, [session?.authenticated, retry]);
  useEffect(() => {
    if (!session?.authenticated) return;
    let alive = true;
    // A container restart invalidates its in-memory sessions. Return to sign-in
    // instead of leaving a stale UI issuing RPCs over a closed connection.
    async function checkSession() {
      try {
        const response = await fetch("/api/session");
        if (!response.ok) return;
        const current = await response.json() as { authenticated: boolean; open: boolean };
        if (alive && !current.authenticated) {
          setError("Your session ended. Sign in again to reconnect to the device.");
          setSession(current);
        }
      } catch { /* A temporary network failure does not establish session expiry. */ }
    }
    const timer = window.setInterval(() => void checkSession(), 15000);
    const onFocus = () => void checkSession();
    window.addEventListener("focus", onFocus);
    return () => { alive = false; window.clearInterval(timer); window.removeEventListener("focus", onFocus); };
  }, [session?.authenticated]);
  async function login(e: React.FormEvent) {
    e.preventDefault(); setBusy(true); setError("");
    try {
      const response = await fetch("/api/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ password }) });
      if (!response.ok) throw new Error(await response.text());
      setPassword(""); setSession({ authenticated: true, open: false });
    } catch (e) { setError(String(e)); } finally { setBusy(false); }
  }
  async function logout() {
    setError("");
    try {
      const response = await fetch("/api/session", { method: "DELETE" });
      if (!response.ok) throw new Error(await response.text());
      client?.dispose(); setClient(undefined); setSession({ authenticated: false, open: false });
    } catch (e) { setError(String(e)); }
  }
  function navigate(next: string) { setView(next); window.history.replaceState(null, "", "#" + encodeURIComponent(next)); setMenu(false); }
  if (!client) return <main className="device-entry"><section className="panel device-login"><Brand /><h1>{session?.authenticated ? "Connecting to this device" : "Device control panel"}</h1><p className="subtle">Applications, hardware and settings, directly on your WendyOS device.</p>{error && <div role="alert" className="inline-error">{error}</div>}{session && !session.authenticated ? <form onSubmit={login}><label htmlFor="password">Device password<input id="password" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus /></label><button className="primary" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button></form> : <p role="status">{busy || !error ? "Loading the device client…" : <button className="primary" onClick={() => setRetry((n) => n + 1)}>Retry connection</button>}</p>}{session?.authenticated && !session.open && <button className="quiet" onClick={() => void logout()}>Sign out</button>}</section></main>;
  const section = sections.find((section) => section.name === view);
  const Icon = icons[view] || Settings;
  return <div className="device-panel">
    <aside className={menu ? "device-sidebar open" : "device-sidebar"}>
      <div className="device-brand"><Brand /><button className="mobile-menu quiet" aria-label="Close navigation" onClick={() => setMenu(false)}><X size={20} /></button></div>
      <div className="device-identity"><span className="status-dot" /><strong>{name}</strong><small>{session?.open ? "Open access" : "Password protected"}</small></div>
      <nav aria-label="Device navigation">{navigationGroups.map((group) => <section className="device-nav-group" key={group.name} aria-labelledby={`nav-${group.name.toLowerCase()}`}><h2 id={`nav-${group.name.toLowerCase()}`}>{group.name}</h2>{group.views.map((item) => { const ItemIcon = icons[item] || Settings; return <button key={item} aria-current={view === item ? "page" : undefined} onClick={() => navigate(item)}><ItemIcon size={17} aria-hidden="true" /><span>{item}</span></button>; })}</section>)}</nav>
      <footer><button className="quiet" onClick={() => setRetry((n) => n + 1)}><RefreshCw size={16} />Reconnect</button>{!session?.open && <button className="quiet" onClick={() => void logout()}><LogOut size={16} />Sign out</button>}</footer>
    </aside>
    <main className="device-main"><header className="device-topbar"><button className="mobile-menu quiet" aria-label="Open navigation" onClick={() => setMenu(true)}><Menu size={20} /></button><Icon size={18} /><span>{view}</span><span className="device-address">{location.host}</span></header>
      {error && <div role="alert" className="inline-error">{error}</div>}
      <div className="device-content">{["Overview", "Logs", "Metrics", "Traces"].includes(view) ? <DeviceDashboard device={{ id: "local", name }} client={client} local requestedView={view} onViewChange={navigate} initialApp={logApp} onBack={() => {}} /> : view === "Terminal" ? <><h1>Device terminal</h1><p className="subtle">An interactive host shell on {name}. Commands run with device administrator access.</p><WendyTerminal device={{ id: "local", name, online: true, version: "" }} client={client} /></> : section && <>
        <div className="page-heading"><div><h1>{section.name}</h1><p className="subtle">{section.description}</p></div><button className="quiet" onClick={() => setRefresh((n) => n + 1)}><RefreshCw size={16} />Refresh</button></div>
        {view === "Updates & drivers" ? <UpdatesView client={client} refresh={refresh} /> : view === "ROS 2" ? <ROSView client={client} refresh={refresh} /> : view === "Mesh" ? <MeshView client={client} refresh={refresh} /> : view === "System logs" ? <SystemLogsView client={client} refresh={refresh} /> : (view === "Sensors & pairing" || view === "Enrollment") ? <ConnectivityView client={client} refresh={refresh} section={view} /> : view === "Storage" ? <StorageView client={client} refresh={refresh} /> : view === "Recordings" ? <RecordingView client={client} refresh={refresh} /> : ["Bluetooth", "Cameras", "Audio"].includes(view) ? <HardwareView key={view} client={client} refresh={refresh} section={view as "Bluetooth" | "Cameras" | "Audio"} /> : view === "Applications" ? <Applications client={client} refresh={refresh} onLogs={(app) => { setLogApp(app); navigate("Logs"); }} /> : view === "Device" ? <DeviceSettings client={client} refresh={refresh} name={name} /> : view === "Network" ? <NetworkSettings client={client} refresh={refresh} /> : <SectionView client={client} operations={section.operations} refresh={refresh} />}
      </>}</div>
    </main>
  </div>;
}
