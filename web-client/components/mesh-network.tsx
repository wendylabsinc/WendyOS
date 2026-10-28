"use client";

import { useEffect, useId, useMemo, useRef, useState } from "react";
import {
  ArrowRight,
  Check,
  ChevronRight,
  CircleHelp,
  Clock3,
  Cpu,
  Network,
  Pause,
  Play,
  RefreshCw,
  ShieldCheck,
  Wifi,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  buildMeshGroups,
  collectMeshSnapshots,
  elapsed,
  isStale,
  mergeMeshObservation,
  meshIdentity,
  nodeState,
  routeState,
  type MeshDevice,
  type MeshGroup,
  type MeshNode,
  type MeshObservation,
  type MeshResult,
  type MeshRoute,
} from "@/lib/mesh-topology";
import styles from "./mesh-network.module.css";

export type MeshDataSource = {
  discover: () => Promise<{ devices: MeshDevice[]; warnings: string[] }>;
  read: (id: string) => Promise<MeshResult>;
};

export default function MeshNetwork({
  source,
  organization,
}: {
  source: MeshDataSource;
  organization?: string;
}) {
  const [devices, setDevices] = useState<MeshDevice[]>([]);
  const [observations, setObservations] = useState<
    Record<string, MeshObservation>
  >({});
  const [error, setError] = useState("");
  const [warnings, setWarnings] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [paused, setPaused] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [now, setNow] = useState(0);
  const [tenant, setTenant] = useState("");
  const [selection, setSelection] = useState("");
  const [showRoutes, setShowRoutes] = useState(true);
  const pending = useRef<Promise<void>>(Promise.resolve());
  useEffect(() => {
    const clock = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(clock);
  }, []);
  useEffect(() => {
    if (paused) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = () => {
      const previous = pending.current;
      const run = async () => {
        // A paused or replaced read cannot cancel an in-flight RPC. Wait for it
        // to settle before launching another batch, including in Strict Mode.
        await previous;
        if (controller.signal.aborted) return;
        setBusy(true);
        setError("");
        try {
          const inventory = await source.discover();
          if (controller.signal.aborted) return;
          setDevices(inventory.devices);
          setWarnings(inventory.warnings);
          setLoaded(true);
          await collectMeshSnapshots(
            inventory.devices,
            source.read,
            (id, result) => {
              const received = Date.now();
              setNow(received);
              setObservations((previous) => ({
                ...previous,
                [id]: mergeMeshObservation(previous[id], result, received),
              }));
            },
            controller.signal,
          );
        } catch (cause) {
          if (!controller.signal.aborted)
            setError(cause instanceof Error ? cause.message : String(cause));
        } finally {
          if (!controller.signal.aborted) {
            setBusy(false);
            timer = setTimeout(() => void poll(), 10_000);
          }
        }
      };
      pending.current = run();
      return pending.current;
    };
    void poll();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [source, paused, refresh]);

  const { groups, unknown } = useMemo(
    () => buildMeshGroups(devices, observations, now),
    [devices, observations, now],
  );
  const group = groups.find((item) => item.tenant === tenant) || groups[0];
  const selected = group?.nodes.find((node) => node.id === selection);
  const fresh =
    group?.nodes.filter(
      (node) =>
        node.observation?.result.status === "ok" &&
        !isStale(node.observation, now),
    ).length || 0;
  const activeLinks =
    group?.links.filter((link) => link.observations.some((item) => !item.stale))
      .length || 0;
  const attention =
    group?.nodes.filter((node) =>
      ["warn", "bad"].includes(nodeState(node, now).tone),
    ).length || 0;
  const received = Object.values(observations).reduce(
    (latest, observation) => Math.max(latest, observation.receivedAt || 0),
    0,
  );
  const waiting = devices.filter((device) => !observations[device.id]).length;
  return (
    <section className={styles.network} aria-label="Local networks">
      <div className={styles.heading}>
        <div>
          <span className={styles.eyebrow}>Connectivity</span>
          <h1>Networks</h1>
          <p>
            Live peer observations and route announcements from your devices.
          </p>
        </div>
        <div className={styles.actions}>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              setPaused(!paused);
              setBusy(false);
            }}
          >
            <>
              {paused ? <Play size={15} /> : <Pause size={15} />}
              {paused ? "Resume" : "Pause"}
            </>
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={busy && !paused}
            onClick={() => {
              setPaused(false);
              setRefresh((value) => value + 1);
            }}
          >
            <RefreshCw
              size={15}
              className={busy && !paused ? "animate-spin" : ""}
            />
            Refresh
          </Button>
        </div>
      </div>
      <div className={styles.liveBar} role="status">
        <span
          className={`${styles.dot} ${paused ? styles.muted : styles.good}`}
        />
        {paused
          ? "Updates paused"
          : busy
            ? "Reading device snapshots"
            : "Auto-refresh every 10 seconds"}
        <span className={styles.separator} />
        Last successful read {elapsed(received, now).toLowerCase()}
        {waiting > 0 && (
          <span className={styles.waiting}>{waiting} pending</span>
        )}
      </div>
      {error && (
        <div className={styles.error} role="alert">
          Could not refresh the device inventory. {error}
          {received > 0 &&
            " Previous observations remain visible and age into stale state."}
        </div>
      )}
      {warnings.map((warning, index) => (
        <div key={index} className={styles.error} role="status">
          {warning}
        </div>
      ))}
      {!groups.length && (
        <div className={styles.empty}>
          <Network size={35} />
          <h2>
            {!loaded && busy
              ? "Reading your networks"
              : "No local net identities observed"}
          </h2>
          <p>
            {!loaded && busy
              ? "Loading devices from your signed-in organization."
              : "A network appears once a device reports its verified organization and device identity. Devices without one are listed below."}
          </p>
          {loaded && !devices.length && (
            <p>No devices were found.</p>
          )}
        </div>
      )}
      {!!groups.length && group && (
        <>
          <div
            className={styles.groupTabs}
            role="group"
            aria-label="Tenant networks"
          >
            {groups.map((item, index) => (
              <button
                type="button"
                aria-pressed={item.tenant === group.tenant}
                className={
                  item.tenant === group.tenant ? styles.activeGroup : ""
                }
                key={item.tenant}
                onClick={() => {
                  setTenant(item.tenant);
                  setSelection("");
                }}
              >
                <ShieldCheck size={16} />
                <span>
                  {groups.length === 1 && organization
                    ? organization
                    : `Tenant ${index + 1}`}
                  <small>{item.tenant.slice(0, 8)}</small>
                </span>
                <span className={styles.count}>{item.nodes.length}</span>
              </button>
            ))}
          </div>
          <div className={styles.metrics}>
            <div>
              <span>Devices in this tenant</span>
              <strong>{group.nodes.length}</strong>
              <small>
                {group.nodes.filter((node) => !node.inventory).length} known
                only through peers
              </small>
            </div>
            <div>
              <span>Fresh snapshots</span>
              <strong>
                {fresh}
                <em>
                  {" "}
                  / {group.nodes.filter((node) => node.inventory).length}
                </em>
              </strong>
              <small>Stale after 30 seconds</small>
            </div>
            <div>
              <span>Observed direct links</span>
              <strong>{activeLinks}</strong>
              <small>From authenticated peer sessions</small>
            </div>
            <div>
              <span>Need attention</span>
              <strong className={attention ? styles.warnText : ""}>
                {attention}
              </strong>
              <small>Stale, unavailable or failed</small>
            </div>
          </div>
          <div className={styles.workspace}>
            <div className={styles.graphPanel}>
              <div className={styles.panelHeader}>
                <div>
                  <h2>Local topology</h2>
                  <p>
                    Tenant <code>{group.tenant}</code>
                  </p>
                </div>
                <label className={styles.toggle}>
                  <input
                    type="checkbox"
                    checked={showRoutes}
                    onChange={(event) => setShowRoutes(event.target.checked)}
                  />
                  Selected routes
                </label>
              </div>
              <Topology
                group={group}
                selected={selected}
                now={now}
                showRoutes={showRoutes}
                onSelect={setSelection}
              />
              <div className={styles.legend}>
                <span>
                  <i className={styles.directLine} />
                  Direct peer session
                </span>
                <span>
                  <i className={styles.routeLine} />
                  Learned destination
                </span>
                <span>
                  <i className={styles.staleLine} />
                  Stale observation
                </span>
              </div>
              <p className={styles.graphNote}>
                <CircleHelp size={14} />
                Partial observations. A link or route does not verify
                application reachability.
              </p>
            </div>
            <aside
              className={styles.details}
              aria-label="Selected device details"
            >
              {selected ? (
                <DeviceDetails
                  node={selected}
                  group={group}
                  now={now}
                  onSelect={setSelection}
                />
              ) : (
                <div className={styles.selectionEmpty}>
                  <Network size={28} />
                  <h2>Inspect a device</h2>
                  <p>
                    Select a node to see its direct peers, learned routes and
                    announcement expiry.
                  </p>
                  <div>
                    <ShieldCheck size={16} />
                    <p>
                      Each graph contains one tenant. Shared WiFi or Ethernet
                      does not imply a local net connection.
                    </p>
                  </div>
                </div>
              )}
            </aside>
          </div>
          <div className={styles.inventory}>
            <div className={styles.panelHeader}>
              <div>
                <h2>Device observations</h2>
                <p>
                  State comes from each device, not from its Cloud heartbeat.
                </p>
              </div>
              <span className={styles.count}>{group.nodes.length}</span>
            </div>
            <div className={styles.tableScroll}>
              <table>
                <thead>
                  <tr>
                    <th>Device</th>
                    <th>Local peer state</th>
                    <th>Direct peers</th>
                    <th>Last read</th>
                    <th>Details</th>
                  </tr>
                </thead>
                <tbody>
                  {group.nodes.map((node) => {
                    const state = nodeState(node, now);
                    return (
                      <tr
                        key={node.id}
                        className={
                          selected?.id === node.id ? styles.selectedRow : ""
                        }
                      >
                        <td>
                          <button
                            className={styles.deviceLink}
                            onClick={() => setSelection(node.id)}
                          >
                            <Cpu size={16} />
                            <span>
                              {node.name}
                              <small>{node.deviceId}</small>
                            </span>
                          </button>
                        </td>
                        <td>
                          <span
                            className={`${styles.state} ${styles[state.tone]}`}
                          >
                            {state.label}
                          </span>
                        </td>
                        <td>
                          {node.observation?.topology
                            ? node.observation.topology.peers?.length || 0
                            : "Unknown"}
                        </td>
                        <td>{elapsed(node.observation?.receivedAt, now)}</td>
                        <td>
                          <button
                            className={styles.inspect}
                            aria-label={`Inspect ${node.name}`}
                            onClick={() => setSelection(node.id)}
                          >
                            <ArrowRight size={16} />
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}
      {!!unknown.length && (
        <div className={styles.inventory}>
          <div className={styles.panelHeader}>
            <div>
              <h2>Identity not available</h2>
              <p>
                These devices cannot be assigned to a tenant graph from the
                current evidence.
              </p>
            </div>
            <span className={styles.count}>{unknown.length}</span>
          </div>
          <div className={styles.unknownList}>
            {unknown.map((device) => (
              <div key={device.id}>
                <Cpu size={17} />
                <span>
                  <strong>{device.name || device.id}</strong>
                  <small>
                    {observations[device.id]?.result.error ||
                      (observations[device.id]?.result.status === "unsupported"
                        ? "This agent does not support topology snapshots."
                        : observations[device.id]
                          ? "No matching canonical local net identity returned."
                          : "Waiting for a topology snapshot.")}
                  </small>
                </span>
                <span className={styles.state}>
                  {observations[device.id]?.result.status || "Not observed"}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}

function Topology({
  group,
  selected,
  now,
  showRoutes,
  onSelect,
}: {
  group: MeshGroup;
  selected?: MeshNode;
  now: number;
  showRoutes: boolean;
  onSelect: (id: string) => void;
}) {
  const marker = useId().replaceAll(":", "");
  const nodes = group.nodes.slice(0, 24);
  if (selected && !nodes.some((node) => node.id === selected.id))
    nodes[nodes.length - 1] = selected;
  const height =
    nodes.length > 8 ? Math.ceil(nodes.length / 4) * 110 + 60 : 510;
  const points = new Map(
    nodes.map((node, index) => {
      const angle = (index / nodes.length) * Math.PI * 2 - Math.PI / 2;
      return [
        node.id,
        nodes.length > 8
          ? { x: 125 + (index % 4) * 223, y: 80 + Math.floor(index / 4) * 110 }
          : {
              x: nodes.length === 1 ? 460 : 460 + Math.cos(angle) * 330,
              y:
                nodes.length === 1
                  ? height / 2
                  : height / 2 + Math.sin(angle) * (height / 2 - 85),
            },
      ];
    }),
  );
  const learned = showRoutes
    ? (selected?.observation?.topology?.routes || []).filter(
        (route) =>
          !route.local &&
          !route.unreachable &&
          route.originPrincipal &&
          route.originPrincipal !== selected?.id,
      )
    : [];
  return (
    <div className={styles.canvas}>
      {group.nodes.length > 24 && (
        <p className={styles.graphLimit}>
          Showing 24 of {group.nodes.length} devices. Select any device in the
          table below.
        </p>
      )}
      <svg
        viewBox={`0 0 920 ${height}`}
        aria-label={`Local topology for tenant ${group.tenant}`}
      >
        <defs>
          <pattern
            id={`${marker}-grid`}
            width="24"
            height="24"
            patternUnits="userSpaceOnUse"
          >
            <circle cx="1" cy="1" r="1" fill="currentColor" opacity=".19" />
          </pattern>
          <marker
            id={`${marker}-arrow`}
            viewBox="0 0 10 10"
            refX="8"
            refY="5"
            markerWidth="5"
            markerHeight="5"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 10 5 L 0 10 z" fill="#b6b3ed" />
          </marker>
        </defs>
        <rect width="920" height={height} fill={`url(#${marker}-grid)`} />
        {group.links.map((link) => {
          const from = points.get(link.source),
            to = points.get(link.target);
          if (!from || !to) return null;
          const stale = link.observations.every((item) => item.stale);
          const focused =
            !selected ||
            selected.id === link.source ||
            selected.id === link.target;
          return (
            <line
              key={link.id}
              x1={from.x}
              y1={from.y}
              x2={to.x}
              y2={to.y}
              className={`${styles.link} ${stale ? styles.staleLink : ""}`}
              opacity={focused ? 1 : 0.25}
            >
              <title>
                {link.observations
                  .map(
                    (item) =>
                      `${group.nodes.find((node) => node.id === item.source)?.name}: ${item.peer.transport || "carrier unknown"}, cost ${item.peer.cost ?? "unknown"}`,
                  )
                  .join("; ")}
                {stale ? "; stale" : ""}
              </title>
            </line>
          );
        })}
        {learned.map((route, index) => {
          const from = points.get(selected!.id),
            to = points.get(
              meshIdentity(route.originPrincipal)?.principal || "",
            );
          if (!from || !to) return null;
          const dx = to.x - from.x,
            dy = to.y - from.y;
          const edge = Math.min(84 / Math.abs(dx), 37 / Math.abs(dy));
          return (
            <path
              key={`${route.prefix}-${index}`}
              d={`M${from.x},${from.y} Q${(from.x + to.x) / 2 - dy * 0.18},${(from.y + to.y) / 2 + dx * 0.18} ${to.x - dx * edge},${to.y - dy * edge}`}
              className={styles.route}
              markerEnd={`url(#${marker}-arrow)`}
              opacity={
                isStale(selected?.observation, now) ||
                routeState(route, now) !== "Authorized at snapshot"
                  ? 0.35
                  : 0.85
              }
            >
              <title>
                Learned destination {route.prefix}; via{" "}
                {route.nextHopAddress || route.nextHopPrincipal || "unknown"}.
                This arc is not a physical link.
              </title>
            </path>
          );
        })}
        {nodes.map((node) => {
          const point = points.get(node.id)!,
            state = nodeState(node, now);
          return (
            <foreignObject
              key={node.id}
              x={point.x - 80}
              y={point.y - 33}
              width="160"
              height="76"
            >
              <button
                type="button"
                className={`${styles.node} ${selected?.id === node.id ? styles.selectedNode : ""}`}
                data-tone={state.tone}
                aria-pressed={selected?.id === node.id}
                onClick={() => onSelect(node.id)}
                aria-label={`${node.name}, ${state.label}. Inspect device.`}
              >
                <span className={styles.nodeTop}>
                  <Cpu size={14} />
                  <strong>{node.name}</strong>
                  <i className={`${styles.dot} ${styles[state.tone]}`} />
                </span>
                <small>{state.label}</small>
              </button>
            </foreignObject>
          );
        })}
      </svg>
    </div>
  );
}

function DeviceDetails({
  node,
  group,
  now,
  onSelect,
}: {
  node: MeshNode;
  group: MeshGroup;
  now: number;
  onSelect: (id: string) => void;
}) {
  const state = nodeState(node, now),
    topology = node.observation?.topology;
  const captured = new Date(Number(topology?.capturedAtUnixMs));
  const routes = (topology?.routes || []).filter((route) => !route.local);
  const announcements = (topology?.announcements || []).filter(
    (item) => item.principal === node.id,
  );
  const name = (principal?: string) =>
    group.nodes.find((item) => item.id === principal)?.name ||
    meshIdentity(principal)?.deviceId.slice(0, 8) ||
    "Unknown";
  return (
    <>
      <div className={styles.detailHeader}>
        <span className={styles.eyebrow}>Device details</span>
        <h2>{node.name}</h2>
        <span className={`${styles.state} ${styles[state.tone]}`}>
          {state.label}
        </span>
      </div>
      <div className={styles.detailBody}>
        {node.observation?.result.error && (
          <p className={styles.detailWarning}>
            {node.observation.result.error}
          </p>
        )}
        {isStale(node.observation, now) && (
          <p className={styles.detailWarning}>
            Showing the last successful snapshot. These links and routes may
            have changed.
          </p>
        )}
        <dl>
          <dt>Platform</dt>
          <dd>{node.observation?.result.platform || node.inventory?.platform || node.inventory?.osType || "Not reported"}</dd>
          <dt>Device identity</dt>
          <dd>{node.deviceId}</dd>
          <dt>Net address</dt>
          <dd>{node.address || "Not reported"}</dd>
          <dt>Snapshot received</dt>
          <dd>{elapsed(node.observation?.receivedAt, now)}</dd>
          <dt>Captured on device</dt>
          <dd>
            {captured.getTime() > 0
              ? captured.toLocaleTimeString()
              : "Not reported"}
          </dd>
        </dl>
        {topology?.status?.detail && (
          <p className={styles.runtimeDetail}>{topology.status.detail}</p>
        )}
        {topology?.truncated && (
          <p className={styles.detailWarning}>
            The device truncated this snapshot. Some peers, routes or
            announcements are omitted.
          </p>
        )}
        <h3>
          Direct peers <span>{topology?.peers?.length || 0}</span>
        </h3>
        {!topology ? (
          <p className={styles.help}>No snapshot from this device.</p>
        ) : !topology.peers?.length ? (
          <p className={styles.help}>No direct peers reported.</p>
        ) : (
          <div className={styles.peers}>
            {topology.peers.map((peer, index) => (
              <button
                key={`${peer.linkId}-${index}`}
                disabled={
                  !group.nodes.some((item) => item.id === peer.principal)
                }
                onClick={() => peer.principal && onSelect(peer.principal)}
              >
                <Wifi size={15} />
                <span>
                  {name(peer.principal)}
                  <small>
                    {peer.transport || "Carrier not reported"} · cost{" "}
                    {peer.cost ?? "unknown"}
                  </small>
                </span>
                <ChevronRight size={15} />
              </button>
            ))}
          </div>
        )}
        <h3>
          Learned routes <span>{routes.length}</span>
        </h3>
        {!routes.length ? (
          <p className={styles.help}>
            {topology ? "No learned routes reported." : "Route state unknown."}
          </p>
        ) : (
          <div className={styles.routes}>
            {routes.map((route, index) => (
              <RouteDetail
                key={`${route.prefix}-${index}`}
                route={route}
                now={now}
                destination={name(route.originPrincipal)}
                nextHop={name(route.nextHopPrincipal)}
              />
            ))}
          </div>
        )}
        {!!announcements.length && (
          <>
            <h3>Own announcement</h3>
            {announcements.map((announcement, index) => (
              <div className={styles.announcement} key={index}>
                <ShieldCheck size={15} />
                <div>
                  <strong>
                    {announcement.trustState === "verified-at-snapshot"
                      ? "Verified at snapshot"
                      : announcement.trustState || "Trust not reported"}
                  </strong>
                  <p>
                    {announcement.expiresAtUnixMs
                      ? expiryText(announcement.expiresAtUnixMs, now)
                      : "Expiry not reported"}
                  </p>
                  <small>Revision {announcement.revision || "0"}</small>
                </div>
              </div>
            ))}
          </>
        )}
        <p className={styles.help}>
          Authorization is reported at capture time. Signed announcements do not
          prove a router forwards traffic.
        </p>
      </div>
    </>
  );
}

function expiryText(expiry: string | undefined, now: number) {
  if (!expiry || !Number(expiry)) return "Proof expiry not reported";
  const remaining = Math.ceil((Number(expiry) - now) / 1000);
  return remaining > 0
    ? `Proof expires in ${remaining}s`
    : `Proof expired ${Math.abs(remaining)}s ago`;
}

function RouteDetail({
  route,
  now,
  destination,
  nextHop,
}: {
  route: MeshRoute;
  now: number;
  destination: string;
  nextHop: string;
}) {
  const state = routeState(route, now);
  return (
    <div className={styles.routeDetail}>
      <strong>{destination}</strong>
      <code>{route.prefix || "Prefix not reported"}</code>
      <p>
        Via {nextHop} <span>Metric {route.metric ?? 0}</span>
      </p>
      <small
        className={state === "Authorized at snapshot" ? "" : styles.warnText}
      >
        {state === "Authorized at snapshot" ? (
          <Check size={12} />
        ) : (
          <CircleHelp size={12} />
        )}
        {state}
      </small>
      <small>
        <Clock3 size={12} />
        {expiryText(route.signedRouteExpiresAtUnixMs, now)}
      </small>
    </div>
  );
}
