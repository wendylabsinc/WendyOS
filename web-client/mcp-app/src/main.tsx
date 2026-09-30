import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { app, extensions, call, share, theme } from "./bridge";
import { Model, ModelLayer } from "./model";
import { AppsPanel, type GatewayApp } from "./apps";
import { CameraPanel } from "./camera";
import { TelemetryPanel } from "./telemetry";
import { SimulatorsPanel } from "./simulators";
import css from "./style.css";
import logoSlate from "./assets/wendy-logo-slate.svg";
import logoCream from "./assets/wendy-logo-cream.svg";
import geist from "./assets/Geist.woff2";
import geistMono from "./assets/GeistMono.woff2";
type Row = {
  id: string;
  name: string;
  model?: string;
  device_type?: string;
  cloud_presence?: string;
  source?: string;
  can_capture?: boolean;
  can_control_apps?: boolean;
  can_read_events?: boolean;
};
type Catalog = {
  robots: Row[];
  selected_robot_id?: string;
  next_offset?: number | null;
  total_count?: number;
  warnings?: string[];
  can_manage_simulators?: boolean;
};
type Inspection = {
  can_open_apps?: boolean;
  model?: string;
  device_type?: string;
  name: string;
  connected: boolean;
  agent_version?: string;
  observed_at?: string;
  apps?: GatewayApp[];
  cameras?: { id: number; name: string }[];
  warnings?: string[];
};
type Trigger = {
  id: string;
  name: string;
  event: string;
  state: string;
  managed: boolean;
  can_configure: boolean;
  plan?: unknown;
};
const style = document.createElement("style");
style.textContent =
  `@font-face{font-family:Geist;src:url("${geist}") format("woff2");font-weight:100 900;font-display:swap}@font-face{font-family:"Geist Mono";src:url("${geistMono}") format("woff2");font-weight:100 900;font-display:swap}` +
  css;
document.head.append(style);
function Workspace() {
  const [ready, setReady] = useState(false),
    [catalog, setCatalog] = useState<Catalog>({ robots: [] }),
    [selected, setSelected] = useState(""),
    [section, setSection] = useState("devices"),
    [query, setQuery] = useState(""),
    [offline, setOffline] = useState(false),
    [three, setThree] = useState(true),
    [tab, setTab] = useState("Overview"),
    [inspection, setInspection] = useState<Inspection>(),
    [triggers, setTriggers] = useState<Trigger[]>([]),
    [events, setEvents] = useState<unknown>(),
    [detail, setDetail] = useState<unknown>(),
    [busy, setBusy] = useState(""),
    [error, setError] = useState(""),
    [needsRefresh, setNeedsRefresh] = useState(false),
    [canSaveSettings, setCanSaveSettings] = useState(false);
  const generation = useRef(0),
    fleetGeneration = useRef(0),
    lastDeepLink = useRef(""),
    identified = useRef(new Set<string>());
  const row = catalog.robots.find((r) => r.id === selected);
  function select(id: string) {
    generation.current++;
    setSelected(id);
    setSection("devices");
    setInspection(undefined);
    setDetail(undefined);
    setEvents(undefined);
    setTriggers([]);
    setBusy("");
    setError("");
    setTab("Overview");
  }
  async function action(label: string, fn: () => Promise<void>) {
    const g = generation.current;
    setBusy(label);
    setError("");
    try {
      await fn();
    } catch (e) {
      if (g === generation.current)
        setError(String(e instanceof Error ? e.message : e));
    } finally {
      if (g === generation.current) setBusy("");
    }
  }
  async function refresh(append = false) {
    const seq = ++fleetGeneration.current;
    await action("Loading devices", async () => {
      const r = await call("list_robots", {
        query,
        include_offline: offline,
        ...(append ? { offset: catalog.next_offset } : {}),
      });
      if (seq !== fleetGeneration.current) return;
      const c = r.structuredContent as unknown as Catalog;
      setCatalog((old) => ({
        ...c,
        robots: append
          ? [
              ...old.robots,
              ...c.robots.filter((r) => !old.robots.some((o) => o.id === r.id)),
            ]
          : c.robots,
      }));
    });
  }
  function route() {
    const raw = extensions.deepLink.getCurrent()?.url;
    if (!raw || raw === lastDeepLink.current) return;
    lastDeepLink.current = raw;
    try {
      const u = new URL(raw, "https://wendy.invalid");
      const p = u.searchParams.get("path") || u.pathname;
      const m = /^\/devices\/([A-Za-z0-9_-]{1,64})$/.exec(p);
      if (m) select(m[1]);
    } catch {
      /* Ignore malformed host paths. */
    }
  }
  useEffect(() => {
    const stale = () => setNeedsRefresh(true);
    window.addEventListener("wendy:refresh-connection", stale);
    app.ontoolresult = (r) => {
      if (r.structuredContent && Array.isArray(r.structuredContent.robots)) {
        const c = r.structuredContent as unknown as Catalog;
        setCatalog(c);
        select(c.selected_robot_id || "");
      }
    };
    app.onhostcontextchanged = () => {
      theme();
      route();
    };
    void app
      .connect()
      .then(async () => {
        theme();
        setReady(true);
        route();
        try {
          const r = await call("read_device_settings"),
            v = r.structuredContent?.values as Record<string, boolean>;
          setCanSaveSettings(r.structuredContent?.can_update === true);
          setThree(v.show_3d);
          setOffline(v.include_offline);
        } catch {
          /* Defaults keep the workspace usable. */
        }
      })
      .catch((e) => setError(String(e)));
    return () => {
      window.removeEventListener("wendy:refresh-connection", stale);
      void app.close();
    };
  }, []);
  useEffect(() => {
    if (!ready || !selected) return;
    const g = generation.current;
    void action("Inspecting device", async () => {
      const r = await call("inspect_robot", { robot_id: selected });
      if (g !== generation.current) return;
      const i = r.structuredContent as unknown as Inspection;
      setInspection(i);
    });
  }, [selected, ready]);
  useEffect(() => {
    if (!ready) return;
    let stopped = false;
    const queue = catalog.robots
      .filter(
        (r) =>
          (!r.model || r.model === "generic") && !identified.current.has(r.id),
      )
      .slice(0, 12);
    for (const r of queue) identified.current.add(r.id);
    const worker = async () => {
      while (queue.length && !stopped) {
        const r = queue.shift()!;
        try {
          const result = await call("identify_device", { robot_id: r.id });
          if (!stopped)
            setCatalog((c) => ({
              ...c,
              robots: c.robots.map((row) =>
                row.id === r.id ? { ...row, ...result.structuredContent } : row,
              ),
            }));
        } catch {
          /* Unknown identity keeps its honest fallback. */
        }
      }
    };
    void Promise.all([worker(), worker(), worker()]);
    return () => {
      stopped = true;
      for (const row of queue) identified.current.delete(row.id);
    };
  }, [ready, catalog.robots.map((r) => r.id).join(",")]);
  async function openTab(t: string) {
    setTab(t);
    setDetail(undefined);
    const g = generation.current;
    if (t === "Events")
      await action("Loading triggers", async () => {
        const r = await call("list_device_triggers", { robot_id: selected });
        if (g === generation.current)
          setTriggers((r.structuredContent?.triggers || []) as Trigger[]);
      });
    if (t === "Metrics" || t === "Logs")
      await action("Reading " + t.toLowerCase(), async () => {
        const r = await call("read_device_" + t.toLowerCase(), {
          robot_id: selected,
        });
        if (g === generation.current) setDetail(r.structuredContent);
      });
  }
  async function attach() {
    await action("Attaching context", async () => {
      await app.updateModelContext({
        content: [
          {
            type: "text",
            text: JSON.stringify({
              robot_id: selected,
              name: row?.name,
              inspection,
            }),
            _meta: { "openai/title": row?.name || selected },
          },
        ],
      });
    });
  }
  const send = (text: string, newChat = false) =>
    action("Sending to ChatGPT", async () => {
      await share(text, newChat);
    });
  const lifecycle = (kind: string) =>
    send(
      `${kind} for Wendy device ${selected || "a new device"}. Use explicit targets, check capabilities, and verify results. ${kind === "Install WendyOS" ? "Start with an installation plan and identify the drive fingerprint. Ask for erase authorization before any write. Preserve vendor Ubuntu on Unitree G1 PC2." : ""}`,
    );
  return (
    <div className="workspace">
      {three && ready && <ModelLayer />}
      <aside>
        <div className="brand">
          <img className="logo-light" src={logoSlate} alt="Wendy" />
          <img className="logo-dark" src={logoCream} alt="Wendy" />
        </div>
        <div className="nav-label">WORKSPACE</div>
        <button
          className={!selected && section === "devices" ? "nav active" : "nav"}
          onClick={() => select("")}
        >
          ▦ <span>Devices</span>
          <span className="count">
            {catalog.total_count ?? catalog.robots.length}
          </span>
        </button>
        <button
          className={section === "simulators" ? "nav active" : "nav"}
          onClick={() => {
            select("");
            setSection("simulators");
          }}
        >
          ◇ <span>Simulators</span>
        </button>
        <div className="nav-label">YOUR DEVICES</div>
        <div className="device-nav">
          {catalog.robots.map((r) => {
            const simulator = r.source === "simulator";
            const online = simulator
              ? r.cloud_presence === "running"
              : r.cloud_presence === "online";
            const presence = online
              ? simulator ? "Local simulator running" : "Cloud online"
              : simulator && r.cloud_presence === "stopped"
                ? "Local simulator stopped"
                : !simulator && r.cloud_presence === "offline"
                  ? "Cloud offline"
                  : "Presence unknown";
            return (
              <button
                key={r.id}
                className={"nav " + (selected === r.id ? "active" : "")}
                onClick={() => select(r.id)}
                title={presence}
                aria-label={`${r.name}: ${presence}`}
              >
                <span className={"dot " + (online ? "online" : "")} aria-hidden="true" />
                <span>{r.name}</span>
              </button>
            );
          })}
        </div>
        <div className="aside-bottom">
          <span className="dot online" />{" "}
          {ready ? "Connected to host" : "Connecting to host"}
          <small>Device access follows your account permissions.</small>
        </div>
      </aside>
      <main>
        <header>
          <div className="breadcrumb">
            <button onClick={() => select("")}>Workspace</button>
            <span>/</span>
            {selected ? row?.name || selected : "Devices"}
          </div>
          <button disabled={!ready || !!busy} onClick={() => refresh()}>
            Refresh fleet
          </button>
        </header>
        <div className="content">
          <div role="status" className="status">
            {busy}
          </div>
          {needsRefresh && (
            <div className="notice" role="status">
              Wendy has new tools. Open ChatGPT Plugins, refresh the Wendy
              connection, and reopen this workspace.
            </div>
          )}
          {error && (
            <div role="alert" className="error">
              {error}
            </div>
          )}
          {catalog.warnings?.map((w) => (
            <div className="notice" key={w}>
              {w}
            </div>
          ))}
          {section === "simulators" ? (
            <SimulatorsPanel
              enabled={!!catalog.can_manage_simulators}
              onChanged={() => {
                void refresh();
              }}
            />
          ) : !selected ? (
            <>
              <div className="page-title">
                <div>
                  <p className="eyebrow">WENDY</p>
                  <h1>Devices</h1>
                  <p>
                    Your authorized devices, applications, and detection events.
                  </p>
                </div>
                <button
                  className="primary"
                  disabled={!ready || !!busy}
                  onClick={() => lifecycle("Install WendyOS")}
                >
                  + Set up a device
                </button>
              </div>
              <div className="toolbar">
                <form
                  onSubmit={(e) => {
                    e.preventDefault();
                    void refresh();
                  }}
                >
                  <input
                    aria-label="Search devices"
                    placeholder="Search your devices"
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                  />
                  <button disabled={!ready || !!busy}>Search</button>
                </form>
                <label>
                  <input
                    type="checkbox"
                    checked={offline}
                    onChange={(e) => setOffline(e.target.checked)}
                  />{" "}
                  Include offline
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={three}
                    onChange={(e) => {
                      const v = e.target.checked;
                      setThree(v);
                      if (canSaveSettings)
                        void action("Saving preference", async () => {
                          await call("update_device_settings", {
                            set: { show_3d: v },
                          });
                        });
                    }}
                  />{" "}
                  3D models
                </label>
              </div>
              <div className="fleet-summary">
                <span>
                  {catalog.total_count ?? catalog.robots.length} devices
                </span>
                <span>
                  Cloud presence shown. Open a device to check its agent.
                </span>
              </div>
              <div className="grid">
                {catalog.robots.map((r) => (
                  <button
                    className="device-card"
                    key={r.id}
                    onClick={() => select(r.id)}
                  >
                    <div className="card-top">
                      <span
                        className={
                          "badge " +
                          (r.cloud_presence === "online" ? "online" : "")
                        }
                      >
                        <i />
                        {r.cloud_presence === "online"
                          ? "Cloud online"
                          : "Presence unknown"}
                      </span>
                      <span>↗</span>
                    </div>
                    {three ? <Model id={r.model} /> : <div className="model" />}
                    <div className="card-info">
                      <h2>{r.name}</h2>
                      <p>{r.device_type || "Wendy device"}</p>
                      <span>
                        {r.can_read_events
                          ? "Detection events enabled"
                          : "Open device workspace"}{" "}
                        →
                      </span>
                    </div>
                  </button>
                ))}
              </div>
              {!catalog.robots.length && (
                <div className="empty">
                  <h2>{ready ? "Find your devices" : "Connecting to Wendy"}</h2>
                  <p>
                    Refresh to load the devices authorized for this account.
                  </p>
                  <button disabled={!ready || !!busy} onClick={() => refresh()}>
                    Load devices
                  </button>
                </div>
              )}
              {catalog.next_offset != null && (
                <button disabled={!!busy} onClick={() => refresh(true)}>
                  Load more devices
                </button>
              )}
              <div className="lifecycle-grid">
                {[
                  [
                    "Develop",
                    "Validate a project and check device compatibility.",
                  ],
                  ["Deploy", "Build and deploy to an explicit device."],
                  [
                    "Scale",
                    "Review targets, start with a canary, and verify readiness.",
                  ],
                ].map(([name, text]) => (
                  <button
                    key={name}
                    onClick={() => lifecycle(name)}
                    disabled={!ready || !!busy}
                  >
                    <span>{name} ↗</span>
                    <p>{text}</p>
                    <small>Continue in ChatGPT</small>
                  </button>
                ))}
              </div>
            </>
          ) : (
            <>
              <div className="page-title">
                <div>
                  <p className="eyebrow">DEVICE WORKSPACE</p>
                  <h1>{row?.name || selected}</h1>
                  <p>
                    {row?.device_type || "Wendy device"} ·{" "}
                    {inspection?.connected
                      ? "Agent connected"
                      : "Connection not verified"}
                  </p>
                </div>
                <div className="actions">
                  <button onClick={attach} disabled={!inspection || !!busy}>
                    Attach to chat
                  </button>
                  {extensions.message && (
                    <button
                      onClick={() =>
                        send(
                          `Help me work with Wendy device ${selected}. Inspect it first.`,
                          true,
                        )
                      }
                      disabled={!!busy}
                    >
                      New chat
                    </button>
                  )}
                </div>
              </div>
              <div className="device-hero">
                {three && <Model id={inspection?.model || row?.model} large />}
                <div className="device-facts">
                  <span className="badge online">
                    {inspection?.connected
                      ? "Agent responded"
                      : "Awaiting inspection"}
                  </span>
                  <h2>Device status</h2>
                  <dl>
                    <dt>Agent</dt>
                    <dd>{inspection?.agent_version || "Unknown"}</dd>
                    <dt>Applications</dt>
                    <dd>{inspection?.apps?.length ?? "Unknown"}</dd>
                    <dt>Last checked</dt>
                    <dd>
                      {inspection?.observed_at
                        ? new Date(inspection.observed_at).toLocaleTimeString()
                        : "Not checked"}
                    </dd>
                  </dl>
                </div>
              </div>
              <nav className="tabs" aria-label="Device details">
                {[
                  "Overview",
                  "Apps",
                  "Camera",
                  "Events",
                  "Metrics",
                  "Logs",
                ].map((t) => (
                  <button
                    key={t}
                    className={tab === t ? "chosen" : ""}
                    onClick={() => openTab(t)}
                    disabled={
                      !!busy ||
                      (t === "Camera" && !row?.can_capture) ||
                      (t === "Events" && !row?.can_read_events)
                    }
                  >
                    {t}
                  </button>
                ))}
              </nav>
              {inspection?.warnings?.map((w) => (
                <div key={w} className="notice">
                  {w}
                </div>
              ))}
              {tab === "Overview" && (
                <div className="lifecycle-grid">
                  {["Develop", "Deploy", "Scale"].map((s) => (
                    <button
                      key={s}
                      disabled={!!busy}
                      onClick={() => lifecycle(s)}
                    >
                      <span>{s} ↗</span>
                      <p>Work with this device in ChatGPT.</p>
                    </button>
                  ))}
                </div>
              )}
              {tab === "Apps" && (
                <AppsPanel
                  apps={inspection?.apps || []}
                  canControl={!!row?.can_control_apps}
                  busy={!!busy}
                  onOpen={
                    inspection?.can_open_apps
                      ? async (a) => {
                          await action("Opening app", async () => {
                            const r = await call("open_robot_app", {
                              robot_id: selected,
                              app_name: a.name,
                            });
                            const url = r.structuredContent?.url;
                            if (typeof url !== "string")
                              throw Error(
                                "The app did not return a web address.",
                              );
                            await app.openLink({ url });
                          });
                        }
                      : undefined
                  }
                  refreshing={!inspection}
                  onToggle={async (a) => {
                    const g = generation.current;
                    await action("Changing app state", async () => {
                      await call(
                        a.state.replace(/^APP_RUNNING_STATE_/, "") === "RUNNING"
                          ? "stop_robot_app"
                          : "start_robot_app",
                        { robot_id: selected, app_name: a.name },
                      );
                      const r = await call("inspect_robot", {
                        robot_id: selected,
                      });
                      if (g === generation.current)
                        setInspection(
                          r.structuredContent as unknown as Inspection,
                        );
                    });
                  }}
                />
              )}
              {tab === "Camera" && inspection && (
                <CameraPanel
                  key={selected}
                  robot={selected}
                  cameras={inspection.cameras || []}
                />
              )}
              {tab === "Events" && (
                <div className="panel">
                  <h2>Detection events</h2>
                  <p>
                    Enable a device trigger, then ask ChatGPT to wait for its
                    next event. Delivery requires a pending wait in the host.
                  </p>
                  {!triggers.length && (
                    <p>No approved triggers on this device.</p>
                  )}
                  {triggers.map((t) => (
                    <div className="trigger" key={t.id}>
                      <div>
                        <strong>{t.name}</strong>
                        <small>
                          {t.event} · {t.state}
                        </small>
                      </div>
                      <div className="actions">
                        {t.managed && t.can_configure && (
                          <>
                            <button
                              disabled={!!busy}
                              onClick={() =>
                                action("Enabling trigger", async () => {
                                  await call("configure_device_trigger", {
                                    robot_id: selected,
                                    trigger_id: t.id,
                                    enabled: true,
                                  });
                                  await openTab("Events");
                                })
                              }
                            >
                              Enable detection
                            </button>
                            <button
                              disabled={!!busy}
                              onClick={() =>
                                action("Disabling inference", async () => {
                                  await call("configure_device_trigger", {
                                    robot_id: selected,
                                    trigger_id: t.id,
                                    enabled: false,
                                  });
                                  await openTab("Events");
                                })
                              }
                            >
                              Disable inference
                            </button>
                          </>
                        )}
                        <button
                          disabled={!!busy}
                          onClick={() => {
                            const g = generation.current;
                            void action("Reading events", async () => {
                              const r = await call("list_device_events", {
                                robot_id: selected,
                                trigger_id: t.id,
                                replay: true,
                              });
                              if (g === generation.current)
                                setEvents(r.structuredContent);
                            });
                          }}
                        >
                          Recent events
                        </button>
                        <button
                          className="primary"
                          disabled={!!busy}
                          onClick={() =>
                            send(
                              `Tell me when ${t.event} happens on Wendy device ${selected}. Use wait_for_device_event with robot_id=${selected} and trigger_id=${t.id}. Use MCP task augmentation if your host supports it. Do not claim an unattended notification is armed unless the host confirms it.`,
                            )
                          }
                        >
                          Tell me when it happens
                        </button>
                      </div>
                    </div>
                  ))}
                  {events != null && (
                    <TelemetryPanel
                      kind="Events"
                      data={events}
                      loading={!!busy}
                    />
                  )}
                </div>
              )}
              {(tab === "Metrics" || tab === "Logs") && (
                <div className="panel">
                  <h2>{tab}</h2>
                  {detail != null ? (
                    <TelemetryPanel kind={tab} data={detail} loading={!!busy} />
                  ) : (
                    <p>No sample loaded.</p>
                  )}
                </div>
              )}
            </>
          )}
          <footer>
            Wendy OS <span>Install · Develop · Deploy · Scale</span>
          </footer>
        </div>
      </main>
    </div>
  );
}
createRoot(document.getElementById("root")!).render(<Workspace />);
