import { useState } from "react";
import { Database, HardDrive, PackageOpen } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections } from "./operations";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import { useResource, ResourceError } from "./Resource";
import { bytes } from "./presentation";
import "./StorageView.css";

type Volume = { name: string; path: string; sizeBytes: string; createdAt?: string; usedBy?: string[] };
type Disk = { usedBytes?: string; totalBytes?: string; mountpoint?: string };
const storageOperations = sections.find((section) => section.name === "Storage")!.operations;
export default function StorageView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [revision, setRevision] = useState(0), [selected, setSelected] = useState<Volume>();
  const volumes = useResource<{ volumes?: Volume[] }>(client, "wendy.agent.services.v2.WendyContainerService", "ListVolumes", refresh + revision);
  const info = useResource<{ containerStorage?: Disk }>(client, "wendy.agent.services.v2.WendyDeviceInfoService", "GetDeviceInfo", refresh + revision);
  const items = volumes.data?.volumes || [], disk = info.data?.containerStorage;
  const total = Number(disk?.totalBytes || 0), used = Number(disk?.usedBytes || 0), volumeBytes = items.reduce((sum, volume) => sum + Number(volume.sizeBytes || 0), 0);
  const percent = total > 0 ? Math.min(100, used / total * 100) : 0;
  const updated = () => setRevision((value) => value + 1);
  return <div className="storage-view">
    <section className="panel storage-capacity">
      <div className="storage-capacity-heading"><div><HardDrive size={22} /><h2>Device storage</h2></div><span>{total > 0 ? `${bytes(Math.max(0, total - used))} available` : info.loading ? "Loading…" : "Capacity unavailable"}</span></div>
      <ResourceError error={info.error} />
      {total > 0 && <><div className="storage-capacity-value"><strong>{bytes(used)}</strong><span>of {bytes(total)} used</span></div><meter min={0} max={100} value={percent} aria-label="Device storage used" /><div className="storage-capacity-foot"><span>{Math.round(percent)}% used</span><span>Applications, images and saved data</span></div></>}
    </section>
    <section className="storage-volume-section">
      <div className="storage-section-heading"><div><h2>Saved data</h2><p>{items.length ? `${items.length} ${items.length === 1 ? "volume" : "volumes"} · ${bytes(volumeBytes)}` : "Persistent storage for your applications."}</p></div></div>
      <ResourceError error={volumes.error} />
      {volumes.loading ? <p className="subtle" role="status">Loading volumes…</p> : !items.length && !volumes.error ? <div className="panel storage-empty"><PackageOpen size={28} /><h3>No saved volumes</h3><p>Application volumes appear here when they are created.</p></div> : <div className="storage-volume-grid">{items.map((volume) => <article className="panel storage-volume-card" key={volume.name}>
        <div className="storage-volume-icon"><Database size={20} /><span>{volume.usedBy?.length ? "In use" : "Not attached"}</span></div>
        <h3>{volume.name}</h3><strong className="storage-volume-size">{bytes(volume.sizeBytes)}</strong>
        <p className="storage-volume-apps">{volume.usedBy?.length ? volume.usedBy.join(", ") : "No applications attached"}</p>
        {total > 0 && <div className="storage-volume-share"><meter min={0} max={total} value={Math.min(total, Number(volume.sizeBytes || 0))} aria-label={`${volume.name} share of device storage`} /><small>{(Number(volume.sizeBytes || 0) / total * 100).toLocaleString(undefined, { maximumFractionDigits: 1 })}% of device capacity</small></div>}
        <div className="storage-volume-actions"><button className="quiet" onClick={() => setSelected(volume)} aria-haspopup="dialog">Details</button><OperationCard client={client} operation={{ ...storageOperations.find((op) => op.method === "RemoveVolume")!, initial: { name: volume.name }, title: `Delete ${volume.name}`, help: "Permanently delete this volume and its saved data." }} refresh={refresh} presentation="header" triggerLabel="Delete" onApplied={updated} /></div>
      </article>)}</div>}
    </section>
    <section className="panel storage-cleanup"><div><h2>Free up space</h2><p>Review unused application cache before removing it.</p></div><div className="storage-cleanup-actions">{storageOperations.filter((op) => op.method === "PruneCache").map((op) => <OperationCard key={op.title} client={client} operation={op} refresh={refresh} presentation="header" triggerLabel={op.read ? "Review cache" : "Clean up"} onApplied={updated} />)}</div></section>
    <Sheet open={Boolean(selected)} title={selected?.name || "Volume"} onClose={() => setSelected(undefined)}>{selected && <dl className="storage-volume-details"><div><dt>Size</dt><dd>{bytes(selected.sizeBytes)}</dd></div><div><dt>Used by</dt><dd>{selected.usedBy?.join(", ") || "No applications"}</dd></div>{selected.createdAt && <div><dt>Created</dt><dd>{Number.isNaN(Date.parse(selected.createdAt)) ? selected.createdAt : new Date(selected.createdAt).toLocaleString()}</dd></div>}<div><dt>Location</dt><dd className="storage-path">{selected.path}</dd></div></dl>}</Sheet>
  </div>;
}
