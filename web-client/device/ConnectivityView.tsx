import { useState, type ReactNode } from "react";
import { Activity, Bot, Cable, Cloud, Fingerprint, Radio, Unplug } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { sections, type Operation } from "./operations";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import { useResource, ResourceError } from "./Resource";
import Result from "./Result";
import "./ConnectivityView.css";

type Props = { client: WendyClient; refresh: number; section: "Sensors & pairing" | "Enrollment" };
type Pairing = { name?: string; sourceAssetId?: number; sourcePrincipalUri?: string; connected?: boolean; sensorAllowlist?: string[] };
type Robot = { serial: string; name?: string; address?: string; connected?: boolean };
type Arm = { id: string; name?: string; model?: string; product?: string; configured?: boolean; connected?: boolean; connectionStatus?: string; devicePath?: string; note?: string };
type Sensor = { channelId: number; name: string; sensor?: { schema?: string; rateHz?: number }; video?: { width?: number; height?: number; fps?: number }; audio?: { channels?: number; sampleRate?: number } };
const ops = (section: Props["section"]) => sections.find((item) => item.name === section)!.operations;
const method = (section: Props["section"], name: string) => ops(section).find((op) => op.method === name)!;
const unavailable = (error?: string) => Boolean(error && /unknown service|unknown method|method .* not implemented/i.test(error));
function Action({ client, refresh, operation, label, initial, onApplied }: { client: WendyClient; refresh: number; operation: Operation; label: string; initial?: Record<string, unknown>; onApplied?: () => void }) {
  return <OperationCard client={client} refresh={refresh} presentation="header" triggerLabel={label} operation={{ ...operation, ...(initial ? { initial } : {}) }} onApplied={onApplied} />;
}
export default function ConnectivityView(props: Props) {
  return props.section === "Enrollment" ? <Enrollment client={props.client} refresh={props.refresh} /> : <Sensors client={props.client} refresh={props.refresh} />;
}
function Sensors({ client, refresh }: Omit<Props, "section">) {
  const [revision, setRevision] = useState(0), [details, setDetails] = useState<{ title: string; value: unknown }>();
  const paired = useResource<{ pairings?: Pairing[] }>(client, method("Sensors & pairing", "ListSensorPairings").service, "ListSensorPairings", refresh + revision);
  const robots = useResource<{ robots?: Robot[] }>(client, method("Sensors & pairing", "ListUnitreePairings").service, "ListUnitreePairings", refresh + revision);
  const arms = useResource<{ devices?: Arm[] }>(client, method("Sensors & pairing", "ListRobotArms").service, "ListRobotArms", refresh + revision);
  const manifest = useResource<{ sensors?: Sensor[] }>(client, method("Sensors & pairing", "GetSensorManifest").service, "GetSensorManifest", refresh + revision);
  const updated = () => setRevision((value) => value + 1);
  const action = (name: string, label: string, initial?: Record<string, unknown>) => <Action client={client} refresh={refresh} operation={method("Sensors & pairing", name)} label={label} initial={initial} onApplied={updated} />;
  const inspect = (title: string, value: unknown) => <button className="quiet" onClick={() => setDetails({ title, value })} aria-haspopup="dialog">Details</button>;
  return <div className="connectivity-view">
    <ConnectionGroup title="SensorLink" description="Sensors paired with this device." icon={<Radio size={21} />} actions={!unavailable(paired.error) && <>{action("DiscoverSensorSources", "Find sensors")}{action("AddSensorPairing", "Pair sensor")}</>}>
      <GroupStatus loading={paired.loading} error={paired.error} empty={!paired.data?.pairings?.length} emptyText="No paired sensors" />
      {paired.data?.pairings?.map((item) => <div className="connectivity-item" key={item.sourcePrincipalUri || item.sourceAssetId}><div className="connectivity-item-label"><span className="connectivity-state" data-connected={Boolean(item.connected)} /><div><h3>{item.name || `Sensor ${item.sourceAssetId || "source"}`}</h3><p>{item.connected ? "Connected" : "Disconnected"}{item.sensorAllowlist?.length ? ` · ${item.sensorAllowlist.length} sensors selected` : ""}</p></div></div><div className="connectivity-actions">{inspect(item.name || "Sensor details", item)}{action("RemoveSensorPairing", "Unpair", item.sourcePrincipalUri ? { sourcePrincipalUri: item.sourcePrincipalUri } : { sourceAssetId: item.sourceAssetId })}</div></div>)}
    </ConnectionGroup>
    <ConnectionGroup title="Unitree robots" description="Robot connections over your local network." icon={<Bot size={21} />} actions={!unavailable(robots.error) && <>{action("DiscoverUnitreeRobots", "Find robots")}{action("PairUnitree", "Pair robot")}</>}>
      <GroupStatus loading={robots.loading} error={robots.error} empty={!robots.data?.robots?.length} emptyText="No paired robots" />
      {robots.data?.robots?.map((item) => <div className="connectivity-item" key={item.serial}><div className="connectivity-item-label"><span className="connectivity-state" data-connected={Boolean(item.connected)} /><div><h3>{item.name || item.serial || "Unitree robot"}</h3><p>{item.connected ? "Connected" : "Disconnected"}{item.address ? ` · ${item.address}` : ""}</p></div></div><div className="connectivity-actions">{inspect(item.name || "Robot details", item)}{action("ForgetUnitree", "Forget", { serial: item.serial })}</div></div>)}
    </ConnectionGroup>
    <ConnectionGroup title="Robot arms" description="USB adapters and configured arms." icon={<Cable size={21} />} actions={!unavailable(arms.error) && action("BindRobotArm", "Pair arm")}>
      <GroupStatus loading={arms.loading} error={arms.error} empty={!arms.data?.devices?.length} emptyText="No robot arm adapters detected" />
      {arms.data?.devices?.map((item) => <div className="connectivity-item" key={item.id}><div className="connectivity-item-label"><span className="connectivity-state" data-connected={Boolean(item.connected)} /><div><h3>{item.name || item.product || "USB serial adapter"}</h3><p>{item.connected ? "USB detected" : item.connectionStatus === "ambiguous" ? "Multiple matching adapters" : "USB disconnected"}{item.model ? ` · ${item.model}` : " · Not configured"}</p></div></div><div className="connectivity-actions">{inspect(item.name || "Arm details", item)}{item.configured ? action("ForgetRobotArm", "Forget", { id: item.id }) : action("BindRobotArm", "Pair", { id: item.id, name: item.name || "" })}</div></div>)}
    </ConnectionGroup>
    {!unavailable(manifest.error) && (manifest.loading || manifest.error || Boolean(manifest.data?.sensors?.length)) && <ConnectionGroup title="On-device sensors" description="Sensor channels reported by this device." icon={<Activity size={21} />} actions={Boolean(manifest.data?.sensors?.length) && action("StreamSensors", "Live values")}>
      <GroupStatus loading={manifest.loading} error={manifest.error} empty={false} emptyText="" />{manifest.data?.sensors?.map((sensor) => <div className="connectivity-item" key={sensor.channelId}><div><h3>{sensor.name || `Channel ${sensor.channelId}`}</h3><p>{sensor.video ? `${sensor.video.width} × ${sensor.video.height} · ${sensor.video.fps} fps` : sensor.audio ? `${sensor.audio.channels} audio channels` : sensor.sensor?.rateHz ? `${sensor.sensor.rateHz} samples per second` : "Sensor"}</p></div><div className="connectivity-actions">{inspect(sensor.name, sensor)}{action("StreamSensors", "Live values", { channelId: [sensor.channelId] })}</div></div>)}</ConnectionGroup>}
    <Sheet open={Boolean(details)} title={details?.title || "Connection details"} onClose={() => setDetails(undefined)}><Result value={details?.value} /></Sheet>
  </div>;
}
function ConnectionGroup({ title, description, icon, actions, children }: { title: string; description: string; icon: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return <section className="panel connectivity-group"><div className="connectivity-group-heading"><div className="connectivity-group-title">{icon}<div><h2>{title}</h2><p>{description}</p></div></div><div className="connectivity-actions">{actions}</div></div><div className="connectivity-group-body">{children}</div></section>;
}
function GroupStatus({ loading, error, empty, emptyText }: { loading: boolean; error?: string; empty: boolean; emptyText: string }) {
  if (loading) return <p className="connectivity-empty" role="status">Loading…</p>;
  if (unavailable(error)) return <p className="connectivity-empty">Unavailable on this agent version.</p>;
  if (error) return <ResourceError error={error} />;
  return empty ? <div className="connectivity-empty"><Unplug size={18} /><span>{emptyText}</span></div> : null;
}
type EnrollmentStatus = {
  provisioned?: { cloudHost?: string; organizationId?: number; assetId?: number; principalUri?: string; assetUuid?: string; localResetSupported?: boolean };
  notProvisioned?: { acmeEnrollmentSupported?: boolean; acmeEnrollmentUnavailableReason?: string };
};
function Enrollment({ client, refresh }: Omit<Props, "section">) {
  const [revision, setRevision] = useState(0);
  const enrollment = useResource<EnrollmentStatus>(client, method("Enrollment", "IsProvisioned").service, "IsProvisioned", refresh + revision);
  const provisioned = enrollment.data?.provisioned, available = enrollment.data?.notProvisioned;
  const updated = () => setRevision((value) => value + 1);
  const action = (name: string, label: string, initial?: Record<string, unknown>) => <Action client={client} refresh={refresh} operation={method("Enrollment", name)} label={label} initial={initial} onApplied={updated} />;
  return <div className="connectivity-view"><section className="panel enrollment-summary"><div className="enrollment-icon"><Cloud size={30} /></div><div><span className="enrollment-eyebrow">Wendy Cloud</span><h2>{enrollment.loading ? "Checking enrollment…" : provisioned ? "This device is enrolled" : available ? "This device is not enrolled" : "Enrollment unavailable"}</h2><p>{provisioned ? "Your organization can access this device through Wendy Cloud." : available ? "Enroll to access and manage this device remotely." : ""}</p><ResourceError error={enrollment.error} />{provisioned?.cloudHost && <div className="enrollment-host">{provisioned.cloudHost}</div>}</div></section>
    {available && <section className="panel enrollment-actions"><div><h2>Enroll this device</h2><p>Use the enrollment details from your organization.</p></div><div className="connectivity-actions">{action("StartProvisioning", "Use enrollment token")}{available.acmeEnrollmentSupported && action("StartACMEProvisioning", "Use registration details")}</div>{available.acmeEnrollmentSupported === false && available.acmeEnrollmentUnavailableReason && <p className="subtle">{available.acmeEnrollmentUnavailableReason}</p>}</section>}
    {provisioned && <><section className="panel connectivity-group"><div className="connectivity-group-heading"><div className="connectivity-group-title"><Fingerprint size={21} /><div><h2>Device identity</h2><p>The identity registered with your organization.</p></div></div></div><dl className="enrollment-identity">{provisioned.assetUuid && <div><dt>Device ID</dt><dd>{provisioned.assetUuid}</dd></div>}{Boolean(provisioned.assetId) && <div><dt>Device ID</dt><dd>{provisioned.assetId}</dd></div>}{Boolean(provisioned.organizationId) && <div><dt>Organization ID</dt><dd>{provisioned.organizationId}</dd></div>}</dl>{provisioned.principalUri && <details className="enrollment-advanced"><summary>Technical identity</summary><code>{provisioned.principalUri}</code></details>}</section><section className="panel enrollment-actions"><div><h2>Disconnect from Wendy Cloud</h2><p>Disconnect this device using the Wendy CLI with your organization account.</p></div></section></>}
  </div>;
}
