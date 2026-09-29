import { useCallback, useEffect, useState } from "react";
import {
  Box,
  Activity,
  CheckCircle2,
  Circle,
  Cpu,
  Download,
  FolderOpen,
  HardDrive,
  Loader2,
  MessageSquare,
  Play,
  Plus,
  RefreshCw,
  Settings2,
  Square,
  TerminalSquare,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import Terminal from "./Terminal";
import SimulatorView from "./SimulatorView";
import RobotGallery from "./RobotGallery";
import DeviceDashboard from "./components/DeviceDashboard";
import DeviceLogs from "./components/DeviceLogs";
import type { DeviceLogSource } from "./log-types";
import type {
  Container,
  DesktopAPI,
  DiscoveredDevice,
  Drive,
  HostStatus,
  InstallPlan,
  ProjectAction,
  Runtime,
  Session,
  Simulator,
  SimulatorKind,
  VirtualMachine,
} from "./types";

const runtimeName = (runtime: Runtime | "qemu") =>
  runtime === "qemu"
    ? "QEMU"
    : runtime === "docker"
      ? "Docker"
      : "Apple Container";
const simulatorName = (kind: SimulatorKind) =>
  kind === "raspberry-pi"
    ? "Raspberry Pi app simulation"
    : kind === "g1"
      ? "G1"
      : "Go2";
const errorMessage = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
type View = "workspace" | "devices" | "containers" | "flash";
type Monitor = { target: string; tab: "dashboard" | "logs"; app?: string };

export default function App() {
  const api = window.wendyDesktop;
  if (!api)
    return (
      <main className="desktop-unavailable">
        <Cpu size={32} />
        <h1>Open Wendy Desktop</h1>
        <p>
          This workspace needs the Electron app to access containers, projects,
          and removable drives.
        </p>
        <code>npm run desktop</code>
      </main>
    );
  return <Workspace api={api} />;
}

function Workspace({ api }: { api: DesktopAPI }) {
  const [view, setView] = useState<View>("workspace");
  const [monitor, setMonitor] = useState<Monitor>();
  const [simLogs, setSimLogs] = useState<{ id: string; name: string }>();
  const [runtime, setRuntime] = useState<Runtime>("docker");
  const [host, setHost] = useState<HostStatus>();
  const [project, setProject] = useState("");
  const [target, setTarget] = useState("docker");
  const [sessions, setSessions] = useState<Session[]>([]);
  const [activeSession, setActiveSession] = useState("");
  const [simulators, setSimulators] = useState<Simulator[]>([]);
  const [selectedSim, setSelectedSim] = useState("");
  const [simName, setSimName] = useState("go2-lab");
  const [simKind, setSimKind] = useState<SimulatorKind>("go2");
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [showTerminal, setShowTerminal] = useState(true);
  const simulator = simulators.find((s) => s.id === selectedSim);
  const onlineRobots = simulators.filter((sim) => sim.reachable && sim.kind !== "raspberry-pi");
  const runtimeStatus = host?.runtimes.find((r) => r.id === runtime);

  const refresh = useCallback(async () => {
    const [host, simulators, sessions] = await Promise.all([
      api.status(),
      api.simulators(),
      api.sessions(),
    ]);
    setHost(host);
    setSimulators(simulators);
    setSelectedSim((selected) =>
      simulators.some((sim) => sim.id === selected)
        ? selected
        : "",
    );
    setSessions(sessions);
  }, [api]);
  useEffect(() => {
    let disposed = false;
    let running = false;
    const update = async () => {
      if (running || disposed) return;
      running = true;
      try {
        const [host, sims, sessions] = await Promise.all([
          api.status(),
          api.simulators(),
          api.sessions(),
        ]);
        if (!disposed) {
          setHost(host);
          setSimulators(sims);
          setSelectedSim((selected) =>
            sims.some((sim) => sim.id === selected)
              ? selected
              : "",
          );
          setSessions(sessions);
        }
      } catch (error) {
        if (!disposed) setError(errorMessage(error));
      } finally {
        running = false;
      }
    };
    void update();
    const interval = setInterval(() => void update(), 8000);
    const unsubscribe = api.onSession((event) => {
      if (event.type === "exit") void update();
    });
    return () => {
      disposed = true;
      clearInterval(interval);
      unsubscribe();
    };
  }, [api]);

  const act = async (name: string, action: () => Promise<unknown>) => {
    setError("");
    setBusy(name);
    try {
      await action();
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy("");
    }
  };
  const openSession = (session: Session | null) => {
    if (!session) return;
    setSessions((previous) => [
      ...previous.filter((s) => s.id !== session.id),
      session,
    ]);
    setActiveSession(session.id);
    setShowTerminal(true);
  };
  const openMonitor = (target: string, tab: Monitor["tab"] = "dashboard") => {
    setMonitor({ target, tab });
    setView("devices");
  };
  const projectAction = (action: ProjectAction["action"]) =>
    act(action, async () =>
      openSession(
        await api.projectAction({ action, project, target, runtime }),
      ),
    );
  const simAction = (action: "start" | "stop" | "retry") =>
    act(action, async () => {
      if (simulator)
        openSession(await api.simulatorAction({ id: simulator.id, action }));
    });

  return (
    <div className="desktop-shell">
      <aside className="desktop-sidebar">
        <div className="desktop-brand">
          <svg viewBox="0 0 100 70" aria-hidden="true">
            <path
              d="M0 35 35 0 70 35 35 70ZM14 35 35 56 56 35 35 14Z"
              fill="currentColor"
            />
            <path d="m35 35 35-35 35 35-35 35Z" fill="currentColor" />
          </svg>
          <strong>wendy</strong>
          <span>DESKTOP</span>
        </div>
        <nav aria-label="Workspace">
          {(
            [
              { id: "workspace", label: "Workspace", icon: Cpu },
              { id: "devices", label: "Devices & monitoring", icon: Activity },
              { id: "containers", label: "Containers", icon: Box },
              { id: "flash", label: "Flash device", icon: Download },
            ] as const
          ).map((item) => (
            <button
              key={item.id}
              className={view === item.id ? "selected" : ""}
              onClick={() => setView(item.id)}
            >
              <item.icon size={18} />
              {item.label}
            </button>
          ))}
        </nav>
        <div className="desktop-sidebar-section">
          <Label htmlFor="runtime">Container runtime</Label>
          <select
            id="runtime"
            value={runtime}
            onChange={(event) => {
              const next = event.target.value as Runtime;
              setRuntime(next);
              if (target === runtime) setTarget(next);
            }}
          >
            <option value="docker">Docker</option>
            <option value="apple-container">Apple Container</option>
          </select>
          <p
            className={
              runtimeStatus?.ready ? "desktop-connected" : "desktop-muted"
            }
          >
            <Circle size={9} fill="currentColor" />
            {!host
              ? "Checking runtime…"
              : runtimeStatus?.ready
                ? "Connected"
                : "Unavailable"}
          </p>
          {runtimeStatus && !runtimeStatus.ready && (
            <>
              <p className="desktop-runtime-error">{runtimeStatus.detail}</p>
              {runtime === "apple-container" && host?.platform === "darwin" && (
                <Button
                  size="sm"
                  variant="outline"
                  disabled={!!busy}
                  onClick={() =>
                    void act("start-apple", async () =>
                      openSession(await api.startApple()),
                    )
                  }
                >
                  Start Apple Container
                </Button>
              )}
            </>
          )}
        </div>
        <div className="desktop-sidebar-section desktop-project">
          <Label>Project</Label>
          <Button
            variant="outline"
            onClick={() =>
              void act("project", async () => {
                const path = await api.openProject();
                if (path) setProject(path);
              })
            }
          >
            <FolderOpen />
            {project ? "Change folder" : "Open folder"}
          </Button>
          <p title={project}>
            {project || "Open a project for chat, builds, and deployment."}
          </p>
        </div>
        <div className="desktop-sidebar-section">
          <Label htmlFor="target">Build and chat target</Label>
          <Input
            id="target"
            list="targets"
            value={target}
            onChange={(event) => setTarget(event.target.value)}
            placeholder="docker, vm:name, device.local"
          />
          <datalist id="targets">
            <option value="docker" />
            <option value="apple-container" />
            {simulators
              .filter((sim) => sim.target)
              .map((sim) => (
                <option key={sim.id} value={sim.target}>
                  {sim.name}
                </option>
              ))}
          </datalist>
          <p className="desktop-muted">
            Use a runtime, Wendy device address, or vm:name.
          </p>
        </div>
        <div className="desktop-sidebar-bottom">
          <span>{host?.version || "Starting Wendy…"}</span>
          <button
            onClick={() => void act("refresh", refresh)}
            aria-label="Refresh connection"
          >
            <RefreshCw size={14} />
          </button>
        </div>
      </aside>
      <main className="desktop-main">
        <header className="desktop-header">
          <div>
            <span className="desktop-eyebrow">LOCAL WORKSPACE</span>
            <h1>
              {view === "flash"
                ? "Flash a device"
                : view === "containers"
                  ? "Your containers"
                  : view === "devices"
                    ? "Devices & monitoring"
                    : "Build with Wendy"}
            </h1>
          </div>
          <div className="desktop-header-actions">
            <span className="desktop-runtime-badge">
              {runtimeName(runtime)}
            </span>
            {busy && <Loader2 className="animate-spin" size={17} />}
          </div>
        </header>
        {error && (
          <div className="desktop-error" role="alert">
            <span>{error}</span>
            <button onClick={() => setError("")} aria-label="Dismiss error">
              <X size={16} />
            </button>
          </div>
        )}
        <div className="desktop-content">
          {view === "workspace" && (
            <>
              <section className="desktop-toolbar" aria-label="Project actions">
                <div>
                  <FolderOpen size={18} />
                  <span>
                    {project
                      ? project.split(/[\\/]/).pop()
                      : "Choose a project folder"}
                  </span>
                </div>
                <div className="desktop-actions">
                  <Button
                    disabled={!project || !!busy}
                    onClick={() => void projectAction("chat")}
                  >
                    <MessageSquare />
                    Chat
                  </Button>
                  <Button
                    variant="outline"
                    disabled={!project || !!busy}
                    onClick={() => void projectAction("build")}
                  >
                    <Box />
                    Build
                  </Button>
                  <Button
                    variant="outline"
                    disabled={!project || !!busy}
                    onClick={() => void projectAction("run")}
                  >
                    <Play />
                    Build & run
                  </Button>
                  <Button
                    variant="ghost"
                    title="Configure chat provider and model"
                    aria-label="Configure chat provider and model"
                    disabled={!project || !!busy}
                    onClick={() => void projectAction("chat-setup")}
                  >
                    <Settings2 />
                  </Button>
                </div>
              </section>
              <section className="desktop-simulator">
                <div className="desktop-section-heading">
                  <div>
                    <Cpu size={19} />
                    <h2>
                      {simulator ? simulatorName(simulator.kind) : "Simulators"}
                    </h2>
                    {simulator && (
                      <span
                        className={
                          simulator.ready
                            ? "desktop-connected"
                            : "desktop-muted"
                        }
                      >
                        {simulator.ready
                          ? simulator.mode || "Ready"
                          : simulator.mode || "Not ready"}
                      </span>
                    )}
                  </div>
                  <div className="desktop-actions">
                    {simulator && onlineRobots.length > 0 && <Button size="sm" variant="ghost" onClick={() => setSelectedSim("")}>All online robots</Button>}
                    {simulators.length > 0 && (
                      <select
                        aria-label="Selected simulator"
                        value={selectedSim}
                        onChange={(event) => setSelectedSim(event.target.value)}
                      >
                        <option value="">All online robots</option>
                        {simulators.map((sim) => (
                          <option value={sim.id} key={sim.id}>
                            {sim.name} · {runtimeName(sim.runtime)}
                          </option>
                        ))}
                      </select>
                    )}
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setCreating(!creating)}
                    >
                      <Plus />
                      New simulator
                    </Button>
                  </div>
                </div>
                {(creating || simulators.length === 0) && (
                  <form
                    className="desktop-create-sim"
                    onSubmit={(event) => {
                      event.preventDefault();
                      void act("create-simulator", async () => {
                        const result = await api.createSimulator({
                          name: simName,
                          runtime,
                          kind: simKind,
                        });
                        setSimulators((previous) => [
                          ...previous,
                          result.simulator,
                        ]);
                        setSelectedSim(result.simulator.id);
                        openSession(result.session);
                        setCreating(false);
                      });
                    }}
                  >
                    <div>
                      <Label htmlFor="sim-kind">Simulation</Label>
                      <select
                        id="sim-kind"
                        value={simKind}
                        onChange={(event) => {
                          const kind = event.target.value as SimulatorKind;
                          setSimKind(kind);
                          setSimName(
                            kind === "raspberry-pi" ? "pi-lab" : `${kind}-lab`,
                          );
                        }}
                      >
                        <option value="go2">Unitree Go2</option>
                        <option value="g1">Unitree G1</option>
                        <option value="raspberry-pi">
                          Raspberry Pi apps · WendyOS VM
                        </option>
                      </select>
                    </div>
                    <div>
                      <Label htmlFor="sim-name">Simulator name</Label>
                      <Input
                        id="sim-name"
                        value={simName}
                        onChange={(event) => setSimName(event.target.value)}
                        pattern="[a-z][a-z0-9-]*"
                        maxLength={48}
                        required
                      />
                    </div>
                    <Button
                      type="submit"
                      disabled={
                        (simKind !== "raspberry-pi" && !runtimeStatus?.ready) ||
                        !!busy
                      }
                    >
                      <Play />
                      Create on{" "}
                      {simKind === "raspberry-pi"
                        ? "QEMU"
                        : runtimeName(runtime)}
                    </Button>
                    <p>
                      {simKind === "raspberry-pi"
                        ? "Run ARM64 apps in WendyOS with 2 CPUs, 2 GB RAM, and a 16 GB virtual disk. Downloads the OS on first use. GPIO and Pi peripherals are not emulated."
                        : "The first launch builds MuJoCo and ROS 2 from pinned sources. Each robot uses 4 CPUs and 4 GB of memory."}
                    </p>
                  </form>
                )}
                {!simulator && onlineRobots.length > 0 ? (
                  <RobotGallery api={api} robots={onlineRobots} onSelect={setSelectedSim} />
                ) : simulator?.reachable ? (
                  <SimulatorView
                    key={simulator.id}
                    api={api}
                    simulator={simulator}
                  />
                ) : (
                  <div className="desktop-viewer-empty">
                    <Cpu size={38} />
                    <h3>
                      {simulator?.kind === "raspberry-pi"
                        ? simulator.ready
                          ? "WendyOS is ready"
                          : "WendyOS virtual machine"
                        : simulator
                          ? `Waiting for ${simulatorName(simulator.kind)}`
                          : "Choose a simulation"}
                    </h3>
                    <p>
                      {simulator?.kind === "raspberry-pi"
                        ? "Build and run Raspberry Pi ARM64 apps, follow device logs, and open Dashboard to inspect CPU, memory, and containers. Hardware peripherals are not emulated."
                        : simulator
                          ? "The live viewer opens when the robot reports healthy and ready. Follow startup in the session below."
                          : "Launch a Go2 or G1 with a live 3D viewer, or a WendyOS virtual machine for Raspberry Pi app development."}
                    </p>
                    {simulator?.error && (
                      <details>
                        <summary>Connection details</summary>
                        <pre>{simulator.error}</pre>
                      </details>
                    )}
                  </div>
                )}
                {simulator && (
                  <div className="desktop-sim-footer">
                    <span>
                      {simulator.name} · {runtimeName(simulator.runtime)}
                      {simulator.target ? ` · ${simulator.target}` : ""}
                    </span>
                    <div className="desktop-actions">
                      {simulator.target && (
                        <>
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={!simulator.ready}
                            onClick={() => openMonitor(simulator.target!)}
                          >
                            <Activity /> Dashboard
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            disabled={!simulator.ready}
                            onClick={() => openMonitor(simulator.target!, "logs")}
                          >
                            Device logs
                          </Button>
                        </>
                      )}
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={!!busy}
                        onClick={() => setSimLogs({ id: simulator.id, name: simulator.name })}
                      >
                        {simulator.kind === "raspberry-pi"
                          ? "Boot logs"
                          : "Logs"}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={!!busy}
                        onClick={() => void simAction("retry")}
                      >
                        {simulator.kind === "raspberry-pi"
                          ? "Retry setup"
                          : "Retry build"}
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={!!busy}
                        onClick={() => void simAction("start")}
                      >
                        <Play />
                        Start
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={!!busy}
                        onClick={() => void simAction("stop")}
                      >
                        <Square />
                        Stop
                      </Button>
                    </div>
                  </div>
                )}
              </section>
            </>
          )}
          {view === "devices" && (
            <Devices
              api={api}
              target={target}
              monitor={monitor}
              onMonitor={setMonitor}
            />
          )}
          {view === "workspace" && simLogs && (
            <section className="desktop-card">
              <div className="desktop-section-heading"><h2>{simLogs.name} logs</h2><Button variant="ghost" size="sm" onClick={() => setSimLogs(undefined)} aria-label="Close simulator logs"><X /></Button></div>
              <DeviceLogs key={simLogs.id} api={api} target={simLogs.name} source={{ kind: "simulator", id: simLogs.id }} />
            </section>
          )}
          {view === "containers" && (
            <Containers key={runtime} api={api} runtime={runtime} />
          )}
          {view === "flash" && <Flash api={api} onSession={openSession} />}
          {view !== "devices" && view !== "containers" && <section
            className={`desktop-sessions ${showTerminal ? "expanded" : ""}`}
          >
            <div className="desktop-section-heading">
              <div>
                <TerminalSquare size={18} />
                <h2>Sessions</h2>
                <span className="desktop-muted">
                  {sessions.filter((s) => s.running).length} active
                </span>
              </div>
              <div className="desktop-actions">
                {activeSession &&
                  sessions.find((s) => s.id === activeSession)?.running && (
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        void act("cancel", () => api.cancel(activeSession))
                      }
                    >
                      <Square />
                      Stop session
                    </Button>
                  )}
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setShowTerminal(!showTerminal)}
                >
                  {showTerminal ? "Collapse" : "Expand"}
                </Button>
              </div>
            </div>
            {showTerminal && (
              <>
                {sessions.length > 0 && (
                  <div
                    className="desktop-session-tabs"
                    role="tablist"
                    aria-label="Terminal sessions"
                  >
                    {sessions.map((session) => (
                      <button
                        role="tab"
                        aria-selected={session.id === activeSession}
                        key={session.id}
                        onClick={() => setActiveSession(session.id)}
                      >
                        <span
                          className={
                            session.running
                              ? "desktop-connected"
                              : session.exitCode
                                ? "desktop-failed"
                                : "desktop-muted"
                          }
                        >
                          {session.running ? (
                            <Circle size={8} fill="currentColor" />
                          ) : (
                            <CheckCircle2 size={12} />
                          )}
                        </span>
                        {session.title}
                      </button>
                    ))}
                  </div>
                )}
                {activeSession ? (
                  <Terminal key={activeSession} api={api} id={activeSession} />
                ) : (
                  <div className="desktop-session-empty">
                    <MessageSquare size={24} />
                    <p>Open a project and start chat, or launch a simulator.</p>
                    <span>
                      Commands and approvals appear here. Sessions stay open
                      when you switch views.
                    </span>
                  </div>
                )}
              </>
            )}
          </section>}
        </div>
      </main>
    </div>
  );
}

function Devices({ api, target, monitor, onMonitor }: {
  api: DesktopAPI;
  target: string;
  monitor?: Monitor;
  onMonitor: (monitor?: Monitor) => void;
}) {
  const [address, setAddress] = useState(monitor?.target || (!["docker", "apple-container", "local"].includes(target) ? target : ""));
  const [vms, setVMs] = useState<VirtualMachine[]>([]);
  const [devices, setDevices] = useState<DiscoveredDevice[]>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const act = async (action: () => Promise<unknown>) => {
    setError("");
    setBusy(true);
    try { await action(); }
    catch (error) { setError(errorMessage(error)); }
    finally { setBusy(false); }
  };
  useEffect(() => {
    let disposed = false;
    void api.virtualMachines().then((vms) => { if (!disposed) setVMs(vms); })
      .catch((error) => { if (!disposed) setError(errorMessage(error)); });
    return () => { disposed = true; };
  }, [api]);
  const open = (selected: string, tab: Monitor["tab"] = "dashboard", app?: string) => {
    const next = selected.trim();
    if (!next) return;
    setAddress(next);
    onMonitor({ target: next, tab, app });
  };
  return <>
    {!monitor && <section className="desktop-card">
      <div className="desktop-section-heading">
        <div><Activity size={20} /><h2>Devices</h2></div>
        <div className="desktop-actions">
          <Button variant="outline" size="sm" disabled={busy} onClick={() => void act(async () => setDevices(await api.discover()))}>{busy ? "Discovering…" : "Discover devices"}</Button>
          <Button variant="outline" size="sm" disabled={busy} onClick={() => void act(async () => setVMs(await api.virtualMachines()))}><RefreshCw />Refresh VMs</Button>
        </div>
      </div>
      <form className="desktop-actions desktop-padding" onSubmit={(event) => { event.preventDefault(); open(address); }}>
        <Label htmlFor="monitor-target">Device address</Label>
        <Input id="monitor-target" className="desktop-target-input" placeholder="pi.local or vm:name" value={address} onChange={(event) => setAddress(event.target.value)} />
        <Button type="submit" disabled={!address.trim()}><Activity />Dashboard</Button>
      </form>
      {error && <p className="desktop-error" role="alert">{error}</p>}
      <div className="desktop-table-wrap"><table><thead><tr><th>Device</th><th>Connection</th><th>Address</th><th><span className="sr-only">Open dashboard</span></th></tr></thead><tbody>
        {vms.map((vm) => <tr key={`vm:${vm.name}`} className={target === `vm:${vm.name}` ? "desktop-selected-row" : undefined}>
          <td>{vm.name}</td><td>Local VM · {vm.state}</td><td>{vm.address || "Not running"}</td><td><Button size="sm" variant="outline" disabled={vm.state !== "running"} onClick={() => open(`vm:${vm.name}`)} aria-label={`Dashboard for ${vm.name}`}>Dashboard</Button></td>
        </tr>)}
        {devices?.map((device) => <tr key={device.target} className={target === device.target ? "desktop-selected-row" : undefined}>
          <td>{device.name}</td><td>{device.transport}</td><td>{device.address}</td><td><Button size="sm" variant="outline" onClick={() => open(device.target)} aria-label={`Dashboard for ${device.name}`}>Dashboard</Button></td>
        </tr>)}
      </tbody></table></div>
      {!vms.length && !devices?.length && <p className="desktop-muted desktop-padding">{devices ? "No network devices or local VMs found. Enter a device address to connect." : "Discover a device, enter its address, or start a local VM."}</p>}
    </section>}
    {monitor && <>
      <div className="desktop-toolbar"><Button variant="ghost" size="sm" onClick={() => onMonitor(undefined)}>← Back to devices</Button><span className="desktop-muted">{monitor.target}</span></div>
      <section className="desktop-card">
      <div className="desktop-monitor-tabs" role="tablist" aria-label={`Monitoring ${monitor.target}`}>
        <button type="button" role="tab" id="dashboard-tab" aria-controls="monitor-panel" aria-selected={monitor.tab === "dashboard"} onClick={() => open(monitor.target)}>Dashboard</button>
        <button type="button" role="tab" id="logs-tab" aria-controls="monitor-panel" aria-selected={monitor.tab === "logs"} onClick={() => open(monitor.target, "logs")}>Logs</button>
      </div>
      <div role="tabpanel" id="monitor-panel" aria-labelledby={monitor.tab === "dashboard" ? "dashboard-tab" : "logs-tab"}>
        {monitor.tab === "dashboard" ? <DeviceDashboard key={monitor.target} api={api} target={monitor.target} onLogs={(app) => open(monitor.target, "logs", app)} /> : <DeviceLogs key={`${monitor.target}:${monitor.app || ""}`} api={api} target={monitor.target} initialApp={monitor.app} />}
      </div>
    </section></>}
  </>;
}

function Containers({
  api,
  runtime,
}: {
  api: DesktopAPI;
  runtime: Runtime;
}) {
  const [logs, setLogs] = useState<{ target: string; source: DeviceLogSource }>();
  const [containers, setContainers] = useState<Container[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [generation, setGeneration] = useState(0);
  useEffect(() => {
    let disposed = false;
    void api
      .containers(runtime)
      .then((items) => {
        if (!disposed) {
          setContainers(items);
          setError("");
        }
      })
      .catch((error) => {
        if (!disposed) setError(errorMessage(error));
      })
      .finally(() => {
        if (!disposed) setLoading(false);
      });
    return () => {
      disposed = true;
    };
  }, [api, runtime, generation]);
  if (logs) return <>
    <div className="desktop-section-heading">
      <Button variant="ghost" size="sm" onClick={() => setLogs(undefined)}>← Back to containers</Button>
    </div>
    <DeviceLogs key={`${logs.source.kind}:${logs.source.id}`} api={api} target={logs.target} source={logs.source} />
  </>;
  return (
    <section className="desktop-card">
      <div className="desktop-section-heading">
        <div>
          <Box size={20} />
          <h2>{runtimeName(runtime)}</h2>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            setLoading(true);
            setGeneration((g) => g + 1);
          }}
        >
          <RefreshCw />
          Refresh
        </Button>
      </div>
      {error ? (
        <p className="desktop-error">{error}</p>
      ) : loading ? (
        <p className="desktop-muted desktop-padding">Reading containers…</p>
      ) : containers.length === 0 ? (
        <p className="desktop-muted desktop-padding">
          No containers on this runtime. Open a project and choose Build & run.
        </p>
      ) : (
        <div className="desktop-table-wrap">
          <table>
            <thead>
              <tr>
                <th>Container</th>
                <th>Image</th>
                <th>State</th>
                <th>Ports</th>
                <th>Logs</th>
              </tr>
            </thead>
            <tbody>
              {containers.map((container) => (
                <tr key={container.id}>
                  <td>{container.name}</td>
                  <td>{container.image}</td>
                  <td>{container.state}</td>
                  <td>{container.ports || "None published"}</td>
                  <td>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setLogs({ target: container.name, source: { kind: "container", runtime, id: container.id } })}
                    >
                      Logs
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function Flash({
  api,
  onSession,
}: {
  api: DesktopAPI;
  onSession: (session: Session | null) => void;
}) {
  const [deviceType, setDeviceType] = useState("raspberry-pi-5");
  const [drives, setDrives] = useState<Drive[]>([]);
  const [drivesScanned, setDrivesScanned] = useState(false);
  const [drive, setDrive] = useState("");
  const [version, setVersion] = useState("");
  const [plan, setPlan] = useState<InstallPlan>();
  const [address, setAddress] = useState("");
  const [verification, setVerification] = useState<Record<string, unknown>>();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [flashed, setFlashed] = useState(false);
  const pi = deviceType.startsWith("raspberry-");
  const act = async (name: string, action: () => Promise<unknown>) => {
    setBusy(name);
    setError("");
    try {
      await action();
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy("");
    }
  };
  const invalidate = () => {
    setPlan(undefined);
    setFlashed(false);
    setVerification(undefined);
  };
  return (
    <section className="desktop-flash">
      <div className="desktop-flash-intro">
        <HardDrive size={25} />
        <div>
          <h2>Install WendyOS</h2>
          <p>
            Select your board and its storage. Review the image and erase scope
            before opening the installer.
          </p>
        </div>
      </div>
      {error && (
        <p role="alert" className="desktop-error">
          {error}
        </p>
      )}
      <div className="desktop-flash-grid">
        <section className="desktop-card desktop-padding">
          <h3>1. Choose the target</h3>
          <Label htmlFor="board">Board</Label>
          <select
            id="board"
            value={deviceType}
            onChange={(event) => {
              setDeviceType(event.target.value);
              setDrive("");
              invalidate();
            }}
          >
            <option value="raspberry-pi-5">Raspberry Pi 5</option>
            <option value="raspberry-pi-4">Raspberry Pi 4</option>
            <option value="raspberry-pi-3">Raspberry Pi 3</option>
            <option value="jetson-orin-nano">
              Jetson Orin Nano developer kit
            </option>
            <option value="jetson-agx-orin">
              Jetson AGX Orin developer kit · NVMe
            </option>
            <option value="jetson-agx-thor">
              Jetson AGX Thor developer kit
            </option>
          </select>
          {pi ? (
            <>
              <div className="desktop-field-heading">
                <Label htmlFor="drive">SD card or USB drive</Label>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={!!busy}
                  onClick={() =>
                    void act("drives", async () => {
                      setDrives(await api.drives());
                      setDrivesScanned(true);
                      setDrive("");
                      invalidate();
                    })
                  }
                >
                  <RefreshCw />
                  Find drives
                </Button>
              </div>
              <select
                id="drive"
                value={drive}
                onChange={(event) => {
                  setDrive(event.target.value);
                  invalidate();
                }}
              >
                <option value="">Choose a drive</option>
                {drives.map((drive) => (
                  <option key={drive.id} value={drive.id}>
                    {drive.id} · {drive.name} ·{" "}
                    {(drive.capacity / 1e9).toFixed(1)} GB
                    {!drive.isExternal ? " · non-removable" : ""}
                  </option>
                ))}
              </select>
              <p className="desktop-muted">
                {drivesScanned && drives.length === 0
                  ? "No flashable drives found. Insert the Pi's SD card or USB drive and try again."
                  : "Insert the Pi's storage into this computer, then find drives. The system drive is excluded by Wendy."}
              </p>
            </>
          ) : (
            <p className="desktop-muted">
              Connect the developer kit in USB recovery mode. The installer
              handles the board&apos;s firmware and OS storage.
            </p>
          )}
          <Label htmlFor="os-version">WendyOS version</Label>
          <Input
            id="os-version"
            value={version}
            placeholder="Latest stable"
            onChange={(event) => {
              setVersion(event.target.value);
              invalidate();
            }}
          />
          <Button
            disabled={!!busy || (pi && !drive)}
            onClick={() =>
              void act("plan", async () => {
                setPlan(await api.plan({ deviceType, drive, version }));
                setFlashed(false);
              })
            }
          >
            {busy === "plan" ? (
              <Loader2 className="animate-spin" />
            ) : (
              <Download />
            )}
            Review installation
          </Button>
        </section>
        <section className="desktop-card desktop-padding">
          <h3>2. Review and flash</h3>
          {plan ? (
            <>
              <dl>
                <dt>Board</dt>
                <dd>{plan.device_type}</dd>
                <dt>Version</dt>
                <dd>{plan.version}</dd>
                <dt>Target</dt>
                <dd>
                  {plan.target
                    ? `${plan.target.id} · ${plan.target.name} · ${(plan.target.capacity_bytes / 1e9).toFixed(1)} GB`
                    : "Connected USB recovery device"}
                </dd>
              </dl>
              <p className="desktop-erase">{plan.erase_scope}</p>
              <details>
                <summary>Image checksum and requirements</summary>
                <code className="desktop-checksum">{plan.artifact_sha256}</code>
                <ul>
                  {plan.requirements.map((line) => (
                    <li key={line}>{line}</li>
                  ))}
                </ul>
              </details>
              <Button
                disabled={!!busy || flashed}
                onClick={() =>
                  void act("flash", async () => {
                    const session = await api.flash(plan.id);
                    if (session) {
                      onSession(session);
                      setFlashed(true);
                    }
                  })
                }
              >
                <Download />
                {flashed ? "Installer opened below" : "Flash device"}
              </Button>
              <p className="desktop-muted">
                Answer the installer&apos;s device name, Wi-Fi, and
                administrator prompts in the session below. Leave the drive
                connected until it finishes.
              </p>
            </>
          ) : (
            <p className="desktop-muted">
              Choose a board and review its installation to resolve the exact
              image and target.
            </p>
          )}
        </section>
      </div>
      <section className="desktop-card desktop-padding desktop-verify">
        <h3>3. Verify first boot</h3>
        <p className="desktop-muted">
          After the installer finishes, insert the storage into the Pi and power
          it on. Enter that device&apos;s address once it has booted.
        </p>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            void act("verify", async () =>
              setVerification(await api.verify({ address, planId: plan?.id })),
            );
          }}
        >
          <Label htmlFor="verify-address">Device address</Label>
          <Input
            id="verify-address"
            value={address}
            onChange={(event) => setAddress(event.target.value)}
            placeholder="my-pi.local"
            required
          />
          <Button disabled={!!busy || !address}>
            {busy === "verify" ? (
              <Loader2 className="animate-spin" />
            ) : (
              <CheckCircle2 />
            )}
            Verify device
          </Button>
        </form>
        {verification && (
          <>
            <p
              className={
                verification.verified ? "desktop-connected" : "desktop-error"
              }
            >
              {verification.verified
                ? "First boot verified"
                : "Verification needs attention"}
            </p>
            <pre>{JSON.stringify(verification, null, 2)}</pre>
          </>
        )}
      </section>
    </section>
  );
}
