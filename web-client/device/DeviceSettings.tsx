import { useState } from "react";
import { Cpu, HardDrive, MemoryStick, Server } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections } from "./operations";
import OperationCard from "./OperationCard";
import Result from "./Result";
import { bytes, formatted, unavailableFeature } from "./presentation";
import { useResource, ResourceError } from "./Resource";
type Info = { os?: string; osVersion?: string; version?: string; boardModel?: string; deviceType?: string; cpuArchitecture?: string; cpuCount?: number; memTotalBytes?: string; uptimeSeconds?: string; partitions?: Partition[]; containerStorage?: Partition; kernelVersion?: string; serialNumber?: string; [key: string]: unknown };
type Partition = { mountpoint: string; usedBytes: string; totalBytes: string; filesystem: string };
type Hardware = { capabilities?: { category: string; description: string; devicePath: string; properties?: Record<string, string> }[] };
const infoService = "wendy.agent.services.v2.WendyDeviceInfoService";
export default function DeviceSettings({ client, refresh, name }: { client: WendyClient; refresh: number; name: string }) {
  const [settingsRefresh, setSettingsRefresh] = useState(0);
  const resourceRefresh = refresh + settingsRefresh;
  const info = useResource<Info>(client, infoService, "GetDeviceInfo", resourceRefresh);
  const hardware = useResource<Hardware>(client, infoService, "ListHardwareCapabilities", resourceRefresh);
  const power = useResource<Record<string, unknown>>(client, "wendy.agent.services.v1.WendyAgentService", "GetPowerMode", resourceRefresh);
  const build = useResource<{ builderEnabled?: boolean; buildkitAvailable?: boolean; nativePlatforms?: string[] }>(client, "wendy.agent.services.v2.WendyBuildService", "GetBuildCapabilities", resourceRefresh);
  const clock = useResource<{ unixNanos?: string }>(client, "wendy.agent.services.v2.WendyTimeSyncService", "GetClock", resourceRefresh);
  const data = info.data, operations = sections.find((s) => s.name === "Device")!.operations;
  const action = (method: string) => <OperationCard client={client} operation={{ ...operations.find((op) => op.method === method)!, ...(method === "SetBuildHostEnabled" ? { title: "Allow builds" } : {}) }} refresh={refresh} presentation="header" onApplied={() => setSettingsRefresh((value) => value + 1)} />;
  const partitions = data?.partitions?.filter((p) => Number(p.totalBytes) > 0 && ["/", "/data"].includes(p.mountpoint)) ?? [];
  const storage = data?.containerStorage;
  const hardwareGroups = Object.groupBy(hardware.data?.capabilities ?? [], (cap) => cap.category);
  return <div className="settings-layout device-settings-layout">
    <ResourceError error={info.error} />
    <div className="device-summary-grid">
      <div className="summary-stat"><Server size={19} /><span>Operating system</span><strong>{data ? `${data.os?.toLowerCase() === "wendyos" ? "WendyOS" : data.os || "Unknown"} ${(data.osVersion || "").replace(/^WendyOS[- ]?/i, "")}` : "Loading…"}</strong></div>
      <div className="summary-stat"><Cpu size={19} /><span>Processor</span><strong>{data ? [data.cpuCount ? `${data.cpuCount} cores` : null, data.cpuArchitecture].filter(Boolean).join(" · ") || "Unknown" : "Loading…"}</strong></div>
      <div className="summary-stat"><MemoryStick size={19} /><span>Memory</span><strong>{data?.memTotalBytes ? bytes(data.memTotalBytes) : "Unknown"}</strong></div>
      <div className="summary-stat"><HardDrive size={19} /><span>Application storage</span><strong>{storage ? `${bytes(Number(storage.totalBytes) - Number(storage.usedBytes))} free` : "Unknown"}</strong></div>
    </div>
    <section className="panel settings-panel"><div className="settings-heading settings-heading-with-action"><div><h2>About this device</h2><p className="subtle">{name}</p></div>{action("SetHostname")}</div>
      <dl className="settings-facts">
        {data?.boardModel && <div><dt>Model</dt><dd>{data.boardModel}</dd></div>}
        <div><dt>Agent version</dt><dd>{data?.version || "Unknown"}</dd></div>
        {data?.uptimeSeconds && <div><dt>Uptime</dt><dd>{formatted(data.uptimeSeconds, "uptimeSeconds")}</dd></div>}
        {clock.data?.unixNanos && <div><dt>Device time</dt><dd>{formatted(clock.data.unixNanos, "unixNanos")}</dd></div>}
      </dl><ResourceError error={clock.error} />
      {data && <details className="advanced-details"><summary>Technical details</summary><Result value={data} /></details>}
    </section>
    {partitions.length > 0 && <section className="panel settings-panel"><div className="settings-heading"><h2>Storage</h2></div><div className="storage-list">{partitions.map((p) => {
      const percent = Math.min(100, Number(p.usedBytes) / Number(p.totalBytes) * 100);
      return <div className="storage-item" key={p.mountpoint}><div><strong>{p.mountpoint === "/" ? "System" : p.mountpoint === "/data" ? "Applications & data" : p.mountpoint}</strong><span>{bytes(p.usedBytes)} of {bytes(p.totalBytes)} used</span></div><meter min="0" max="100" value={percent} aria-label={`${p.mountpoint} storage used`} /><small>{Math.round(percent)}% used · {p.filesystem}</small></div>;
    })}</div></section>}
    {power.data && <section className="panel settings-panel"><div className="settings-heading settings-heading-with-action"><h2>Power</h2>{action("SetPowerMode")}</div><div className="settings-body"><Result value={power.data} /></div></section>}
    {power.error && !unavailableFeature(power.error, "power") && <ResourceError error={power.error} />}
    <section className="panel settings-panel"><div className="settings-heading settings-heading-with-action"><div><h2>Build host</h2><p className="subtle">Allow this device to build applications.</p></div>{action("SetBuildHostEnabled")}</div><ResourceError error={build.error} />{build.data && <dl className="settings-facts"><div><dt>Status</dt><dd>{build.data.builderEnabled ? "Enabled" : "Disabled"}</dd></div><div><dt>Build engine</dt><dd>{build.data.buildkitAvailable ? "Available" : "Not installed"}</dd></div></dl>}</section>
    <section className="panel settings-panel"><div className="settings-heading"><h2>Hardware</h2><p className="subtle">Devices detected by WendyOS.</p></div><ResourceError error={hardware.error} />{hardware.loading && <p className="settings-body subtle">Loading hardware…</p>}{Object.entries(hardwareGroups).map(([category, items]) => items?.length ? <details className="hardware-group" key={category}><summary><span>{category.charAt(0).toUpperCase() + category.slice(1)}</span><span className="count-label">{items.length}</span></summary><div className="hardware-items">{items.map((item, i) => <div key={item.devicePath + i}><span>{item.description || item.devicePath}</span><small>{item.devicePath}</small></div>)}</div></details> : null)}</section>
  </div>;
}
