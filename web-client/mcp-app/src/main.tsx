import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  app,
  APP_WEB_REQUEST_OPTIONS,
  extensions,
  call,
  share,
  theme,
  toolErrorMessage,
} from "./bridge";
import { Model, SimulatorIcon } from "./model";
import { AppsPanel, type GatewayApp } from "./apps";
import { CameraPanel } from "./camera";
import { TelemetryPanel } from "./telemetry";
import { SimulatorsPanel } from "./simulators";
import {
  InspectionStatus,
  inspectionLabel,
  type InspectionPhase,
} from "./inspection-status";
import {
  displayIdentity,
  withDisplayIdentity,
  type DisplayIdentity,
} from "./fleet-identity";
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
  can_deploy_detector?: boolean;
};
type Catalog = {
  robots: Row[];
  include_offline?: boolean;
  selected_robot_id?: string;
  next_offset?: number | null;
  total_count?: number;
  simulator_count?: number;
  warnings?: string[];
  discovery_complete?: boolean;
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
    [tab, setTab] = useState("Overview"),
    [inspection, setInspection] = useState<Inspection>(),
    [inspectionPhase, setInspectionPhase] =
      useState<InspectionPhase>("waiting"),
    [inspectionError, setInspectionError] = useState(""),
    [inspectionAttempt, setInspectionAttempt] = useState(0),
    [triggers, setTriggers] = useState<Trigger[]>([]),
    [events, setEvents] = useState<unknown>(),
    [detail, setDetail] = useState<unknown>(),
    [busy, setBusy] = useState(""),
    [error, setError] = useState(""),
    [needsRefresh, setNeedsRefresh] = useState(false);
  const generation = useRef(0),
    actionGeneration = useRef(0),
    fleetGeneration = useRef(0),
    loadedFilters = useRef({ query: "", includeOffline: false }),
    tabReadController = useRef<AbortController | undefined>(undefined),
    selectedRef = useRef(""),
    identities = useRef(new Map<string, DisplayIdentity>()),
    preferencesTouched = useRef({ offline: false }),
    lastDeepLink = useRef("");
  const row = catalog.robots.find((r) => r.id === selected);
  const devices = catalog.robots.filter((r) => r.source !== "simulator");
  const deviceCount = Math.max(
    0,
    (catalog.total_count ?? catalog.robots.length) -
      (catalog.simulator_count ?? catalog.robots.length - devices.length),
  );
  function select(id: string) {
    setSection("devices");
    if (id === selectedRef.current) return;
    selectedRef.current = id;
    generation.current++;
    tabReadController.current?.abort();
    tabReadController.current = undefined;
    setSelected(id);
    setInspection(undefined);
    setInspectionPhase(ready ? "loading" : "waiting");
    setInspectionError("");
    setDetail(undefined);
    setEvents(undefined);
    setTriggers([]);
    setBusy("");
    setError("");
    setTab("Overview");
  }
  function rememberIdentity(id: string, value: unknown) {
    const identity = displayIdentity(value);
    if (!identity) return;
    identities.current.set(id, identity);
    setCatalog((c) => ({
      ...c,
      robots: c.robots.map((r) => (r.id === id ? { ...r, ...identity } : r)),
    }));
  }
  function preserveIdentity(c: Catalog): Catalog {
    return {
      ...c,
      robots: c.robots.map((r) =>
        withDisplayIdentity(r, identities.current.get(r.id)),
      ),
    };
  }
  async function action(label: string, fn: () => Promise<void>) {
    const g = generation.current;
    const currentAction = ++actionGeneration.current;
    setBusy(label);
    setError("");
    try {
      await fn();
    } catch (e) {
      if (
        g === generation.current &&
        currentAction === actionGeneration.current
      )
        setError(toolErrorMessage(e));
    } finally {
      if (
        g === generation.current &&
        currentAction === actionGeneration.current
      )
        setBusy("");
    }
  }
  async function refresh(append = false, includeOffline = offline) {
    preferencesTouched.current.offline = true;
    const appendToLoaded =
      append &&
      loadedFilters.current.query === query &&
      loadedFilters.current.includeOffline === includeOffline;
    const seq = ++fleetGeneration.current;
    await action("Loading devices", async () => {
      const r = await call("list_robots", {
        query,
        include_offline: includeOffline,
        ...(appendToLoaded ? { offset: catalog.next_offset } : {}),
      });
      if (seq !== fleetGeneration.current) return;
      const c = preserveIdentity(r.structuredContent as unknown as Catalog);
      loadedFilters.current = { query, includeOffline };
      setCatalog((old) => ({
        ...c,
        robots: appendToLoaded
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
    const settingsController = new AbortController();
    let settingsTimer: ReturnType<typeof setTimeout> | undefined;
    const stale = () => setNeedsRefresh(true);
    window.addEventListener("wendy:refresh-connection", stale);
    app.ontoolresult = (r) => {
      if (r.structuredContent && Array.isArray(r.structuredContent.robots)) {
        const c = r.structuredContent as unknown as Catalog;
        loadedFilters.current = {
          query: "",
          includeOffline: c.include_offline === true,
        };
        setCatalog(preserveIdentity(c));
        select(c.selected_robot_id || "");
      }
    };
    app.onhostcontextchanged = () => {
      theme();
      route();
    };
    void app
      .connect()
      .then(() => {
        if (settingsController.signal.aborted) return;
        theme();
        setReady(true);
        route();
        settingsTimer = setTimeout(() => {
          void (async () => {
            try {
              const r = await call(
                "read_device_settings",
                {},
                {
                  priority: "background",
                  signal: settingsController.signal,
                  timeout: 6000,
                  resetTimeoutOnProgress: false,
                },
              );
              if (settingsController.signal.aborted) return;
              const v = r.structuredContent?.values as
                | Record<string, unknown>
                | undefined;
              if (
                !preferencesTouched.current.offline &&
                typeof v?.include_offline === "boolean"
              )
                setOffline(v.include_offline);
            } catch {
              /* Preferences must not block the device workspace. */
            }
          })();
        }, 200);
      })
      .catch((e) => setError(String(e)));
    return () => {
      clearTimeout(settingsTimer);
      settingsController.abort();
      tabReadController.current?.abort();
      window.removeEventListener("wendy:refresh-connection", stale);
      void app.close();
    };
  }, []);
  useEffect(() => {
    if (!ready || !selected) return;
    const g = generation.current;
    const controller = new AbortController();
    setInspectionPhase("loading");
    setInspectionError("");
    setInspection(undefined);
    void (async () => {
      try {
        const r = await call(
          "inspect_robot",
          { robot_id: selected },
          {
            signal: controller.signal,
            timeout: 50_000,
            resetTimeoutOnProgress: false,
          },
        );
        if (controller.signal.aborted || g !== generation.current) return;
        const i = r.structuredContent as unknown as Inspection;
        if (!i || typeof i.connected !== "boolean")
          throw Error(
            "The device returned no valid inspection result. Try checking its agent again.",
          );
        setInspection(i);
        setInspectionPhase("complete");
        rememberIdentity(selected, i);
      } catch (error) {
        if (controller.signal.aborted || g !== generation.current) return;
        setInspectionPhase("error");
        setInspectionError(
          toolErrorMessage(error) ||
            "Update the Wendy connection using the instructions above, then retry inspection.",
        );
      }
    })();
    return () => controller.abort();
  }, [selected, ready, inspectionAttempt]);
  async function readDeviceTab(
    name:
      | "inspect_robot"
      | "list_device_triggers"
      | "list_device_events"
      | "read_device_notifications"
      | "read_device_metrics"
      | "read_device_logs",
    args: Record<string, unknown> = {},
  ) {
    if (!selected || selected !== selectedRef.current) return;
    const controller = tabReadController.current ?? new AbortController();
    tabReadController.current = controller;
    try {
      return await call(
        name,
        { ...args, robot_id: selected },
        {
          signal: controller.signal,
          timeout: 50_000,
          resetTimeoutOnProgress: false,
        },
      );
    } catch (error) {
      if (!controller.signal.aborted) throw error;
    }
  }
  async function openTab(t: string) {
    // Mutation completion callbacks may belong to a device we have left.
    if (selected !== selectedRef.current) return;
    setTab(t);
    if (t !== tab) setDetail(undefined);
    const g = generation.current;
    if (t === "Events")
      await action("Loading triggers", async () => {
        const r = await readDeviceTab("list_device_triggers");
        if (r && g === generation.current)
          setTriggers((r.structuredContent?.triggers || []) as Trigger[]);
      });
    if (t === "Metrics" || t === "Logs")
      await action("Reading " + t.toLowerCase(), async () => {
        const r = await readDeviceTab(
          t === "Metrics" ? "read_device_metrics" : "read_device_logs",
        );
        if (r && g === generation.current) setDetail(r.structuredContent);
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
      kind === "Install WendyOS"
        ? "Help me install WendyOS. First identify the target hardware model and connection method with me. Then use the matching installation plan and an explicit target. If that plan writes an OS image, identify the destination drive and obtain erase authorization before writing. Verify the result."
        : `${kind} for Wendy device ${selected || "a new device"}. Use explicit targets, check capabilities, and verify results.`,
    );
  return (
    <div className="workspace">
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
            {deviceCount}
          </span>
        </button>
        <button
          className={section === "simulators" ? "nav active" : "nav"}
          onClick={() => {
            select("");
            setSection("simulators");
          }}
        >
          <SimulatorIcon className="simulator-nav-icon" />{" "}
          <span>Simulators</span>
        </button>
        <div className="nav-label">YOUR DEVICES</div>
        <div className="device-nav">
          {devices.map((r) => {
            const online = r.cloud_presence === "online";
            const presence = online
              ? "Cloud online"
              : r.cloud_presence === "offline"
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
                <span
                  className={"dot " + (online ? "online" : "")}
                  aria-hidden="true"
                />
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
              <div>
                <strong>Update the Wendy connection</strong>
                <p>
                  Open ChatGPT Plugins, select Wendy, and scroll to Manage app.
                  Click Refresh tools, then close this tab and open Wendy in a
                  new conversation. Refresh fleet only updates the device list.
                </p>
                <button
                  onClick={() =>
                    void app
                      .openLink({ url: "https://chatgpt.com/plugins" })
                      .catch((e) => setError(toolErrorMessage(e)))
                  }
                >
                  Open ChatGPT Plugins
                </button>
              </div>
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
                  onClick={() => send("Help me get started with Wendy. Ask what I want to build and whether I have hardware or want a simulator, then guide setup. Check what is installed and which local or hosted connection is available. Verify my first connection before continuing.")}
                >
                    + Get started
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
                    onChange={(e) => {
                      preferencesTouched.current.offline = true;
                      const value = e.target.checked;
                      setOffline(value);
                      void refresh(false, value);
                    }}
                  />{" "}
                  Include offline
                </label>
              </div>
              <div className="fleet-summary">
                <span>
                  {deviceCount} {deviceCount === 1 ? "device" : "devices"}
                </span>
                <span>
                  Open a device to check its connection.
                </span>
              </div>
              <div className="grid">
                {devices.map((r) => (
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
                    <Model
                      id={r.model}
                      name={r.name}
                    />
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
              {!devices.length && (
                <div className="empty">
                  <h2>{!ready ? "Connecting to Wendy" : query.trim() ? "No matching devices" : catalog.discovery_complete === false ? "Device list unavailable" : "Start with a device or simulator"}</h2>
                  <p>
                    {query.trim() ? "Try another name or clear your search." : catalog.discovery_complete === false ? "Check the connection warning and refresh to try again." : "Wendy helps you build and run apps on robots and small computers. Connect your first device, or try a simulator before your hardware arrives."}
                  </p>
                  {ready && !query.trim() && catalog.discovery_complete !== false && (
                    <button className="primary" disabled={!!busy} onClick={() => send("Help me get started. I have not used Wendy before. Help me choose a physical device or simulator, check whether I need local tools or a hosted connection, and verify the result.")}>
                      Help me get started
                    </button>
                  )}
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
                    {inspectionLabel(inspectionPhase, inspection?.connected)}
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
                <Model
                  id={inspection?.model || row?.model}
                  name={row?.name || inspection?.name}
                  kind={row?.source === "simulator" ? "simulator" : undefined}
                  large
                />
                <InspectionStatus
                  phase={inspectionPhase}
                  inspection={inspection}
                  error={inspectionError}
                  ready={ready}
                  onRetry={() => setInspectionAttempt((attempt) => attempt + 1)}
                />
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
              {tab === "Apps" && inspection && (
                <AppsPanel
                  key={selected}
                  apps={inspection?.apps || []}
                  canControl={!!row?.can_control_apps}
                  busy={!!busy}
                  onOpen={
                    inspection?.can_open_apps
                      ? async (a) => {
                          const g = generation.current;
                          const requestedAt = Date.now();
                          const r = await call(
                            "open_robot_app",
                            { robot_id: selected, app_name: a.name },
                            APP_WEB_REQUEST_OPTIONS,
                          );
                          const url = r.structuredContent?.url;
                          if (typeof url !== "string")
                            throw Error(
                              "The app did not return a web address.",
                            );
                          if (g !== generation.current) return;
                          const parsed = new URL(url);
                          if (
                            parsed.protocol !== "http:" ||
                            parsed.hostname !== "127.0.0.1" ||
                            parsed.username ||
                            parsed.password
                          )
                            throw Error(
                              "The gateway returned an unsupported app web address.",
                            );
                          const lifetime =
                            r.structuredContent?.expires_in_seconds;
                          if (
                            typeof lifetime !== "number" ||
                            lifetime <= 0 ||
                            lifetime > 1800
                          )
                            throw Error(
                              "The app web address has no valid expiry.",
                            );
                          return {
                            url: parsed.href,
                            expiresAt: requestedAt + lifetime * 1000,
                          };
                        }
                      : undefined
                  }
                  refreshing={inspectionPhase === "loading"}
                  onToggle={async (a) => {
                    const g = generation.current;
                    await action("Changing app state", async () => {
                      await call(
                        a.state.replace(/^APP_RUNNING_STATE_/, "") === "RUNNING"
                          ? "stop_robot_app"
                          : "start_robot_app",
                        { robot_id: selected, app_name: a.name },
                      );
                      if (g !== generation.current) return;
                      const r = await readDeviceTab("inspect_robot");
                      if (r && g === generation.current)
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
              {(tab === "Apps" || tab === "Camera") && !inspection && (
                <div className="panel" role="status">
                  <h2>{tab}</h2>
                  <p>
                    {inspectionPhase === "error"
                      ? `${tab === "Apps" ? "The app inventory" : "Camera information"} could not be loaded. Retry inspection above.`
                      : `${tab === "Apps" ? "Applications" : "Available cameras"} will appear when device inspection completes.`}
                  </p>
                </div>
              )}
              {tab === "Events" && (
                <div className="panel">
                  <h2>Wendy Data notifications</h2>
                  <p>
                    Ask ChatGPT to monitor detections or named events and choose
                    how it should respond. Monitoring is active once ChatGPT
                    confirms the subscription.
                  </p>
                  <div className="actions">
                    <button
                      onClick={() =>
                        send(
                          `Monitor Wendy Data notifications on device ${selected}. Subscribe to the MCP event wendy.data.notification with robot_id=${selected}. Ask which campaign or event to monitor and what to do when it arrives. Confirm monitoring only after the subscription succeeds.`,
                        )
                      }
                    >
                      Ask ChatGPT to monitor
                    </button>
                    {row?.can_deploy_detector && (
                      <button
                        onClick={() =>
                          send(
                            `Set up a YOLO detector on Wendy device ${selected} using deploy_yolo_detector. Ask me for the Hugging Face model reference and what to detect. Inspect this device's camera sources and ask which camera to use if there is more than one. Inspect the detector after deployment. Offer to subscribe to its Wendy Data notifications.`,
                          )
                        }
                      >
                        Set up YOLO detection
                      </button>
                    )}
                    <button
                      disabled={!!busy}
                      onClick={() => {
                        const g = generation.current;
                        void action("Reading notifications", async () => {
                          const r = await readDeviceTab(
                            "read_device_notifications",
                            { replay: true },
                          );
                          if (r && g === generation.current)
                            setEvents(r.structuredContent);
                        });
                      }}
                    >
                      Recent notifications
                    </button>
                  </div>
                  <p className="muted">
                    Requires an updated Wendy Agent. Detection and named-event
                    notifications are supported; Cloud episode-upload
                    notifications are not included.
                  </p>
                  {triggers.length > 0 && <h3>Configured triggers</h3>}
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
                              const r = await readDeviceTab(
                                "list_device_events",
                                {
                                  trigger_id: t.id,
                                  replay: true,
                                },
                              );
                              if (r && g === generation.current)
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
                              `Tell me when ${t.event} happens on Wendy device ${selected}. Check that this trigger emits Wendy Data notifications, then subscribe to the MCP event wendy.data.notification with robot_id=${selected} and event=${t.event}. Confirm monitoring only after the subscription succeeds.`,
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
                  <div className="actions telemetry-heading">
                    <h2>{tab}</h2>
                    <button disabled={!!busy} onClick={() => void openTab(tab)}>
                      {busy === "Reading " + tab.toLowerCase()
                        ? "Refreshing…"
                        : "Refresh sample"}
                    </button>
                  </div>
                  {detail != null || busy === "Reading " + tab.toLowerCase() ? (
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
