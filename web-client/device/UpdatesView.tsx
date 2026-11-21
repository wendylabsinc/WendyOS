import { useEffect, useState } from "react";
import { CheckCircle2, Cpu, Download, HardDrive, PackageCheck, RefreshCw, ShieldAlert } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections } from "./operations";
import OperationCard from "./OperationCard";
import { useResource, ResourceError } from "./Resource";
import { bytes, errorMessage } from "./presentation";
import { rpc } from "./rpc";
import "./UpdatesView.css";

type AgentUpdate = { installedVersion?: string; latestVersion?: string; updateAvailable?: boolean; phase?: string; detail?: string; downloadedBytes?: string; totalBytes?: string };
type OSUpdate = { hasResult?: boolean; outcome?: string; newOsVersion?: string; note?: string; rollbackError?: string; createdAtUnix?: string };
type Driver = { name: string; version?: string; kernelVersion?: string; loaded?: boolean; unreadable?: boolean; modulesLoad?: string[] };
type Drivers = { installed?: Driver[]; baseVersion?: string; kernelVersion?: string; loadedModules?: string[]; mergedExtensions?: string[] };
const operations = sections.find((section) => section.name === "Updates & drivers")!.operations;
const operation = (method: string) => operations.find((item) => item.method === method)!;
const phases: Record<string, string> = { idle: "Not checked yet", checking: "Checking for updates", available: "Update available", current: "Up to date", downloading: "Downloading update", installing: "Installing update", restarting: "Restarting agent", failed: "Update failed" };
const outcomes: Record<string, string> = { OUTCOME_COMMITTED: "Last update completed", OUTCOME_ROLLED_BACK: "Previous version restored", OUTCOME_ROLLBACK_FAILED: "Rollback failed", OUTCOME_COMMIT_FAILED: "Update could not be finalized", OUTCOME_UNSPECIFIED: "Update result unavailable" };
export default function UpdatesView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [revision, setRevision] = useState(0), [checking, setChecking] = useState(false), [checkError, setCheckError] = useState("");
  const info = useResource<{ osVersion?: string; version?: string }>(client, "wendy.agent.services.v2.WendyDeviceInfoService", "GetDeviceInfo", refresh + revision);
  const agent = useResource<AgentUpdate>(client, operation("GetOnlineUpdateStatus").service, "GetOnlineUpdateStatus", refresh + revision);
  const os = useResource<OSUpdate>(client, operation("GetOSUpdateStatus").service, "GetOSUpdateStatus", refresh + revision);
  const drivers = useResource<Drivers>(client, operation("ListDrivers").service, "ListDrivers", refresh + revision);
  const updated = () => setRevision((value) => value + 1);
  const agentBusy = ["checking", "downloading", "installing", "restarting"].includes(agent.data?.phase || "");
  useEffect(() => {
    if (!agentBusy) return;
    const timer = setTimeout(() => setRevision((value) => value + 1), 3000);
    return () => clearTimeout(timer);
  }, [agentBusy, revision]);
  async function check() {
    setChecking(true); setCheckError("");
    try { await rpc(client, operation("CheckOnlineUpdate"), {}); updated(); }
    catch (error) { setCheckError(errorMessage(error)); }
    finally { setChecking(false); }
  }
  const action = (method: string, triggerLabel: string, initial?: Record<string, unknown>) => <OperationCard client={client} operation={{ ...operation(method), ...(initial ? { initial } : {}) }} refresh={refresh} presentation="header" triggerLabel={triggerLabel} onApplied={updated} />;
  const details = (method: string) => <OperationCard client={client} operation={{ ...operation(method), automatic: false }} refresh={refresh} presentation="header" triggerLabel="Details" />;
  const agentReady = Boolean(agent.data?.updateAvailable && agent.data.latestVersion && !agentBusy);
  const agentFailed = agent.data?.phase === "failed", osFailed = Boolean(os.data?.hasResult && os.data.outcome && /ROLLED_BACK|FAILED/.test(os.data.outcome));
  const items = drivers.data?.installed || [], total = Number(agent.data?.totalBytes || 0), downloaded = Number(agent.data?.downloadedBytes || 0);
  return <div className="updates-view">
    <div className="software-updates-grid">
      <section className="panel software-update-card"><header><div className="update-card-title"><HardDrive size={22} /><h2>WendyOS</h2></div>{action("UpdateOS", "Update")}</header>
        <div className="software-version"><span>Installed version</span><strong>{info.data?.osVersion?.replace(/^WendyOS[- ]?/i, "") || (info.loading ? "Loading…" : "Not reported")}</strong></div>
        <ResourceError error={info.error || os.error} />
        <div className={`software-status ${osFailed ? "error" : os.data?.outcome === "OUTCOME_COMMITTED" ? "good" : ""}`}>{osFailed ? <ShieldAlert size={16} /> : os.data?.outcome === "OUTCOME_COMMITTED" ? <CheckCircle2 size={16} /> : <RefreshCw size={16} />}<span>{os.loading ? "Reading update status…" : os.data?.hasResult ? outcomes[os.data.outcome || ""] || "Update result reported" : os.error ? "Update status unavailable" : "No previous update recorded"}</span></div>
        {os.data?.hasResult && Number(os.data.createdAtUnix) > 0 && <p className="update-note">{new Date(Number(os.data.createdAtUnix) * 1000).toLocaleString()}</p>}
        {(os.data?.note || os.data?.rollbackError) && <p className="update-note">{os.data.rollbackError || os.data.note}</p>}
        <footer><span>Operating system</span>{details("GetOSUpdateStatus")}</footer>
      </section>
      <section className="panel software-update-card"><header><div className="update-card-title"><Cpu size={22} /><h2>Wendy Agent</h2></div><div className="update-card-actions"><button className="quiet" disabled={checking || agentBusy} onClick={() => void check()}><RefreshCw size={14} className={checking ? "animate-spin" : ""} />{checking ? "Checking…" : "Check updates"}</button>{agentReady && action("StartOnlineUpdate", "Install", { version: agent.data!.latestVersion })}</div></header>
        <div className="software-version"><span>Installed version</span><strong>{agent.data?.installedVersion || info.data?.version || (agent.loading ? "Loading…" : "Not reported")}</strong></div>
        <ResourceError error={checkError || agent.error} />
        <div className={`software-status ${agentFailed ? "error" : agent.data?.phase === "current" ? "good" : ""}`}>{agentFailed ? <ShieldAlert size={16} /> : agent.data?.phase === "current" ? <CheckCircle2 size={16} /> : <Download size={16} />}<span>{checking ? "Checking for updates" : agent.loading ? "Reading update status…" : agent.error ? "Update status unavailable" : phases[agent.data?.phase || "idle"] || agent.data?.phase}</span></div>
        {agentReady && <p className="update-note">Version {agent.data!.latestVersion} is available.</p>}
        {agent.data?.detail && <p className="update-note">{agent.data.detail}</p>}
        {total > 0 && agentBusy && <div className="update-download"><progress max={total} value={Math.min(total, downloaded)} aria-label="Agent update download" /><span>{bytes(downloaded)} of {bytes(total)}</span></div>}
        <footer><span>Device services and applications</span>{details("GetOnlineUpdateStatus")}</footer>
      </section>
    </div>
    <section className="panel drivers-panel"><header><div className="update-card-title"><PackageCheck size={22} /><div><h2>Hardware drivers</h2><p>Installed add-ons for this device.</p></div></div>{action("InstallDriver", "Install driver")}</header><ResourceError error={drivers.error} />
      {drivers.loading ? <p className="drivers-empty subtle" role="status">Loading drivers…</p> : !items.length && !drivers.error ? <div className="drivers-empty"><PackageCheck size={28} /><h3>No additional drivers installed</h3><p>Drivers included with WendyOS do not appear in this list.</p></div> : <div className="driver-list">{items.map((driver) => <article className="driver-row" key={driver.name}><div className="driver-icon"><PackageCheck size={21} /></div><div className="driver-summary"><h3>{driver.name}</h3><p>{driver.version ? `Version ${driver.version}` : "Version not reported"}{driver.kernelVersion ? ` · Kernel ${driver.kernelVersion}` : ""}</p><span className={`software-status ${driver.unreadable ? "error" : driver.loaded ? "good" : ""}`}>{driver.unreadable ? "Driver could not be read" : driver.loaded ? "Loaded" : "Installed"}</span></div>{action("RemoveDriver", "Remove", { name: driver.name })}</article>)}</div>}
      <footer><span>{drivers.data?.kernelVersion ? `Kernel ${drivers.data.kernelVersion}` : ""}</span>{details("ListDrivers")}</footer>
    </section>
  </div>;
}
