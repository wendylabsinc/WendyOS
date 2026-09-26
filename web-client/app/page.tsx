"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import { logSeverity, meetsLogLevel } from "@/lib/telemetry";
import {
  Activity,
  ArrowDownToLine,
  ArrowRight,
  ArrowUpRight,
  Box,
  Check,
  ChevronRight,
  CircleHelp,
  Cpu,
  FileText,
  LayoutGrid,
  Loader2,
  LockKeyhole,
  LogOut,
  Monitor,
  MoreHorizontal,
  Pause,
  Play,
  Plus,
  Radio,
  RefreshCw,
  Search,
  ShieldCheck,
  Square,
  Terminal,
  Unplug,
  Wifi,
  X,
  AlertTriangle,
  Zap,
  Network,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Sidebar,
  SidebarProvider,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuItem,
  SidebarMenuButton,
  SidebarTrigger,
} from "@/components/ui/sidebar";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@/components/ui/dialog";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from "@/components/ui/sheet";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
  AlertDialogAction,
} from "@/components/ui/alert-dialog";
import {
  CommandDialog,
  CommandInput,
  CommandList,
  CommandGroup,
  CommandItem,
  CommandEmpty,
} from "@/components/ui/command";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from "@/components/ui/select";
import {
  Table,
  TableHeader,
  TableHead,
  TableBody,
  TableRow,
  TableCell,
} from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Progress } from "@/components/ui/progress";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

import {
  WendyClient,
  formatBytes,
  relativeTime,
  downloadText,
} from "@/lib/client";
import type { View, Device, App, Log, ActivityEntry } from "@/lib/client";
import WendyLogin from "@/components/wendy-login";
import type { AuthSession } from "@/lib/auth";

const nav = [
  { name: "Overview", icon: LayoutGrid },
  { name: "Devices", icon: Cpu },
  { name: "Applications", icon: Box },
  { name: "Logs", icon: Activity },
  { name: "Metrics", icon: Activity },
  { name: "Traces", icon: Activity },
] as const;
const descriptions: Record<View, string> = {
  Overview: "Your devices, applications, and what needs your attention.",
  Devices: "A home for every device on your edge.",
  Applications: "Start, stop, and inspect what runs on your devices.",
  Logs: "Follow what is happening, one line at a time.",
  Metrics: "Select a cloud device to inspect its OpenTelemetry metrics.",
  Traces: "Select a cloud device to inspect its OpenTelemetry traces.",
};
function Brand() {
  return (
    <div className="brand">
      <svg viewBox="0 0 246.82 181.81" fill="currentColor" aria-hidden="true">
        <rect
          x="91.64"
          y="26.62"
          width="128.56"
          height="128.56"
          transform="translate(-18.61 136.88) rotate(-45)"
        />
        <path d="M69.93,160.83L0,90.9,69.93,20.98l69.93,69.93-69.93,69.93ZM22.63,90.9l47.3,47.3,47.3-47.3-47.3-47.3-47.3,47.3Z" />
      </svg>
      wendy<span className="brand-edition">CLIENT</span>
    </div>
  );
}
function Status({ value }: { value: string }) {
  const live = ["Connected", "Running"].includes(value);
  return (
    <span
      className={
        "tag " + (live ? "live" : value === "Needs attention" ? "warning" : "")
      }
    >
      {live && <span className="status-dot" />}
      {value}
    </span>
  );
}
function Sparkline({
  values,
  color = "#b8f5ce",
  height = 40,
}: {
  values: number[];
  color?: string;
  height?: number;
}) {
  if (!values.length) return <span className="subtle">No samples yet</span>;
  const max = Math.max(...values, 40);
  const points = values
    .map(
      (v, i) =>
        `${(i * 160) / Math.max(1, values.length - 1)},${height - 4 - (v / max) * (height - 8)}`,
    )
    .join(" ");
  return (
    <svg
      className="sparkline"
      viewBox={`0 0 160 ${height}`}
      role="img"
      aria-label={
        "CPU history: latest " + Math.round(values.at(-1) || 0) + " percent"
      }
    >
      <path
        d={`M 0 ${height} L ${points.replaceAll(" ", " L ")} L 160 ${height} Z`}
        fill={color}
        opacity=".06"
      />
      <polyline
        points={points}
        fill="none"
        stroke={color}
        strokeWidth="1.7"
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
function DeviceCard({
  device: d,
  apps,
  onClick,
}: {
  device: Device;
  apps: number;
  onClick: () => void;
}) {
  const I = d.model.includes("Jetson")
    ? Cpu
    : d.model.includes("Pi")
      ? Radio
      : Monitor;
  return (
    <button className="panel device-card" onClick={onClick}>
      <div className="device-card-top">
        <span
          className={
            "device-icon " +
            (!d.online ? "off" : d.model.includes("Pi") ? "blue" : "")
          }
        >
          <I size={23} />
        </span>
        <Status value={d.online ? "Connected" : "Offline"} />
      </div>
      <div className="device-name">
        {d.name}
        <ArrowUpRight size={17} />
      </div>
      <p className="subtle">{d.model}</p>
      {d.online ? (
        <div className="card-telemetry">
          <div>
            <span className="eyebrow">CPU</span>
            <p>
              {d.cpu === null ? "—" : Math.round(d.cpu) + "%"}
              <small>
                {" "}
                / {apps} {apps === 1 ? "app" : "apps"}
              </small>
            </p>
          </div>
          <Sparkline
            values={d.history}
            color={d.model.includes("Pi") ? "#a6c4f5" : "#b8f5ce"}
          />
        </div>
      ) : (
        <div className="card-telemetry offline-note">
          <Unplug size={16} />
          No active connection
        </div>
      )}
      <div className="device-footer">
        <span className="actions" style={{ gap: 6 }}>
          <Cpu size={13} />
          {d.online
            ? formatBytes(d.memory) + " / " + formatBytes(d.memoryTotal)
            : "Last seen " + relativeTime(d.updated).toLowerCase()}
        </span>
        <span>
          {d.live ? "Live" : "Disconnected"}{" "}
          {d.online && (
            <>
              <span className="divider">·</span>
              {d.temperature === null ? "WendyOS" : d.temperature + "°C"}
            </>
          )}
        </span>
      </div>
    </button>
  );
}
function FleetSignal({ devices }: { devices: Device[] }) {
  const online = devices.filter((d) => d.online).slice(0, 2);
  return (
    <div className="fleet-visual">
      <svg
        viewBox="0 0 390 180"
        role="img"
        aria-label={`${online.length} connected devices`}
      >
        <defs>
          <pattern
            id="dots"
            width="16"
            height="16"
            patternUnits="userSpaceOnUse"
          >
            <circle cx="1" cy="1" r="1" fill="#48594d" />
          </pattern>
        </defs>
        <rect width="390" height="180" fill="url(#dots)" opacity=".55" />
        {online.map((d, i) => (
          <g key={d.id}>
            <path
              d={`M90 ${i ? 140 : 40} H180 V90 H285`}
              fill="none"
              stroke="#729780"
              strokeWidth="1.5"
            />
            <circle
              cx="90"
              cy={i ? 140 : 40}
              r="20"
              fill="#283b2e"
              stroke="#739f83"
            />
            <circle cx="90" cy={i ? 140 : 40} r="5" fill="#b8f5ce" />
            <text
              x="122"
              y={i ? 166 : 28}
              fill="#c1d4c6"
              fontSize="12"
              letterSpacing="1"
            >
              {d.name.toUpperCase().slice(0, 20)}
            </text>
          </g>
        ))}
        <rect x="262" y="64" width="52" height="52" rx="13" fill="#b8f5ce" />
        <path
          d="m278 91 7 7 14-15"
          fill="none"
          stroke="#183622"
          strokeWidth="3"
        />
        <text x="255" y="144" fill="#c1d4c6" fontSize="12" letterSpacing="1">
          WORKSPACE
        </text>
      </svg>
    </div>
  );
}
function Pick({
  value,
  onChange,
  items,
  label,
}: {
  value: string;
  onChange: (v: string) => void;
  items: { id: string; name: string }[];
  label: string;
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger aria-label={label} className="picker">
        <SelectValue placeholder={label} />
      </SelectTrigger>
      <SelectContent>
        {items.map((i) => (
          <SelectItem key={i.id} value={i.id}>
            {i.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
function Empty({
  icon: Icon = Box,
  title,
  children,
}: {
  icon?: typeof Box;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="empty-state">
      <Icon size={30} />
      <h2>{title}</h2>
      {children}
    </div>
  );
}

export default function Home() {
  const [view, setView] = useState<View>("Overview");
  const [auth, setAuth] = useState<AuthSession | null>(null);
  const login = useRef<{ signOut: () => void }>(null);
  const [devices, setDevices] = useState<Device[]>([]);
  const [apps, setApps] = useState<App[]>([]);
  const [logs, setLogs] = useState<Log[]>([]);
  const [activity, setActivity] = useState<ActivityEntry[]>([]);
  const [commandOpen, setCommandOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [detailId, setDetailId] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<{
    app: App;
    action: "stop" | "restart";
  } | null>(null);
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const [deviceFilter, setDeviceFilter] = useState("all");
  const [logDevice, setLogDevice] = useState("");
  const [logApp, setLogApp] = useState("all");
  const [severity, setSeverity] = useState("INFO");
  const [logSearch, setLogSearch] = useState("");
  const [paused, setPaused] = useState(false);
  const [logError, setLogError] = useState("");
  const [cloudActive, setCloudActive] = useState<{ name: string } | null>(null);
  const [busy, setBusy] = useState("");
  const [refreshing, setRefreshing] = useState(false);
  const [deviceErrors, setDeviceErrors] = useState<Record<string, string>>({});
  const [createOpen, setCreateOpen] = useState(false);
  const [newApp, setNewApp] = useState("");
  const [newImage, setNewImage] = useState("");
  const [newEnv, setNewEnv] = useState("");
  const [newDevice, setNewDevice] = useState("");
  const [createError, setCreateError] = useState("");
  const clients = useRef(new Map<string, WendyClient>());
  const inFlight = useRef(new Set<string>());
  const devicesRef = useRef(devices);
  devicesRef.current = devices;
  const logEnd = useRef<HTMLDivElement>(null);
  const navigate = useCallback((next: View) => {
    setView(next);
    setSearch("");
    setCommandOpen(false);
    if (typeof window !== "undefined")
      window.history.replaceState(null, "", "#" + next.toLowerCase());
  }, []);
  const addActivity = useCallback(
    (message: string, detail: string) =>
      setActivity((a) =>
        [
          {
            id: crypto.randomUUID(),
            message,
            detail,
            time: new Date().toISOString(),
          },
          ...a,
        ].slice(0, 30),
      ),
    [],
  );
  const refresh = useCallback(async (id: string) => {
    const client = clients.current.get(id);
    const current = devicesRef.current.find((d) => d.id === id);
    if (!client || !current || inFlight.current.has(id)) return;
    inFlight.current.add(id);
    try {
      const data = await client.snapshot(current);
      if (clients.current.get(id) !== client) return;
      setDevices((ds) => ds.map((d) => (d.id === id ? data.device : d)));
      setApps((a) => [...a.filter((x) => x.deviceId !== id), ...data.apps]);
      setDeviceErrors((e) => ({ ...e, [id]: data.warnings.join("\n") }));
    } catch (e) {
      if (clients.current.get(id) !== client) return;
      setDevices((ds) =>
        ds.map((d) => (d.id === id ? { ...d, online: false } : d)),
      );
      setDeviceErrors((errors) => ({
        ...errors,
        [id]: e instanceof Error ? e.message : String(e),
      }));
    } finally {
      inFlight.current.delete(id);
    }
  }, []);
  useEffect(() => {
    const hash = window.location.hash.slice(1);
    const route = nav.find((n) => n.name.toLowerCase() === hash);
    if (route) setView(route.name);
    const key = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setCommandOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, []);
  useEffect(() => {
    const all = clients.current;
    return () => {
      all.forEach((c) => c.dispose());
    };
  }, []);
  useEffect(() => {
    const tick = setInterval(() => {
      clients.current.forEach((_, id) => void refresh(id));
    }, 5000);
    return () => clearInterval(tick);
  }, [refresh]);
  useEffect(() => {
    if (view !== "Logs" || paused) return;
    const c = clients.current.get(logDevice);
    if (!c) return;
    setLogError("");
    const unsubscribe = c.onEvent(({ event, data }) => {
      if (event === "logs-error") setLogError(String(data));
      if (event !== "logs") return;
      const rows = parseLogs(data, logDevice);
      setLogs((old) => {
        const ids = new Set(old.map((l) => l.id));
        return [...old, ...rows.filter((l) => !ids.has(l.id))].slice(-500);
      });
    });
    void c
      .call("logs", { app: logApp === "all" ? "" : logApp })
      .catch((e) => setLogError(e.message));
    return () => {
      unsubscribe();
      void c.call("logs-stop").catch(() => {});
    };
  }, [view, paused, logDevice, logApp]);
  useEffect(() => {
    if (view === "Logs" && !paused)
      logEnd.current?.scrollIntoView({ block: "nearest" });
  }, [logs, view, paused]);
  const webState = useRef({ view, devices, apps, navigate });
  webState.current = { view, devices, apps, navigate };
  useEffect(() => {
    type Tool = {
      name: string;
      description: string;
      inputSchema: object;
      annotations: { readOnlyHint: boolean };
      execute: (i: unknown) => unknown;
    };
    const context = (
      document as Document & {
        modelContext?: {
          registerTool: (
            t: Tool,
            o: { signal: AbortSignal },
          ) => void | Promise<void>;
        };
      }
    ).modelContext;
    if (!context) return;
    const life = new AbortController();
    const tools: Tool[] = [
      {
        name: "read_wendy_workspace",
        description:
          "Read device connection states and application statuses in the current Wendy workspace.",
        inputSchema: {
          type: "object",
          properties: {},
          additionalProperties: false,
        },
        annotations: { readOnlyHint: true },
        execute: () => ({
          view: webState.current.view,
          devices: webState.current.devices.map((d) => ({
            id: d.id,
            name: d.name,
            online: d.online,
            connected: !!d.live,
          })),
          apps: webState.current.apps,
        }),
      },
      {
        name: "navigate_wendy_workspace",
        description:
          "Open a Wendy workspace view. Does not run device commands.",
        inputSchema: {
          type: "object",
          properties: {
            view: { type: "string", enum: nav.map((n) => n.name) },
          },
          required: ["view"],
          additionalProperties: false,
        },
        annotations: { readOnlyHint: false },
        execute: (i: unknown) => {
          const v = (i as { view?: string })?.view;
          if (!nav.some((n) => n.name === v)) throw new Error("Unknown view");
          flushSync(() => webState.current.navigate(v as View));
          return { view: v };
        },
      },
    ];
    for (const t of tools) {
      try {
        void Promise.resolve(
          context.registerTool(t, { signal: life.signal }),
        ).catch(() => {});
      } catch {}
    }
    return () => life.abort();
  }, []);
  function disconnect(d: Device) {
    clients.current.get(d.id)?.dispose();
    clients.current.delete(d.id);
    setDevices((ds) => ds.filter((x) => x.id !== d.id));
    setApps((a) => a.filter((x) => x.deviceId !== d.id));
    setDetailId(null);
    const next = devices.find((x) => x.id !== d.id)?.id || "";
    setLogDevice(next);
    setNewDevice(next);
    addActivity(d.name + " disconnected", "Browser session ended");
    toast.success("Disconnected from " + d.name);
  }
  async function appAction(app: App, action: "start" | "stop" | "restart") {
    const d = devices.find((d) => d.id === app.deviceId);
    if (!d?.online) {
      toast.error("This device is offline.");
      return;
    }
    setBusy(app.id);
    try {
      const client = clients.current.get(d.id);
      if (!client) throw new Error("Connect to this device first.");
      await client.call(action, { app: app.name });
      await refresh(d.id);
      addActivity(
        app.name +
          " " +
          (action === "stop"
            ? "stopped"
            : action === "restart"
              ? "restarted"
              : "started"),
        d.name,
      );
      toast.success(
        app.name +
          " " +
          (action === "stop"
            ? "stopped"
            : action === "restart"
              ? "restarted"
              : "started"),
      );
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy("");
      setConfirm(null);
    }
  }
  async function createApp(e: React.FormEvent) {
    e.preventDefault();
    setCreateError("");
    const d = devices.find((d) => d.id === newDevice);
    if (!d?.online) {
      setCreateError("Choose a connected device.");
      return;
    }
    if (!/^[a-z0-9][a-z0-9._-]*$/.test(newApp)) {
      setCreateError(
        "Use lowercase letters, numbers, dots, hyphens, or underscores for the app name.",
      );
      return;
    }
    if (apps.some((a) => a.deviceId === newDevice && a.name === newApp)) {
      setCreateError("An app with this name already exists on the device.");
      return;
    }
    const env = newEnv
      .split("\n")
      .map((v) => v.trim())
      .filter(Boolean);
    if (env.some((v) => !/^\w+=/.test(v))) {
      setCreateError("Enter environment variables as KEY=value, one per line.");
      return;
    }
    setBusy("create");
    try {
      const c = clients.current.get(d.id);
      if (!c) throw new Error("Connect to this device first.");
      await c.call("create", { app: newApp, image: newImage, env });
      await refresh(d.id);
      addActivity(newApp + " created", d.name);
      toast.success("Application created. Start it when you are ready.");
      setCreateOpen(false);
      setNewApp("");
      setNewImage("");
      setNewEnv("");
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy("");
    }
  }
  function openLogs(d: string, app = "all") {
    setLogDevice(d);
    setLogApp(app);
    setDetailId(null);
    navigate("Logs");
  }

  const online = devices.filter((d) => d.online);
  const running = apps.filter((a) => a.status === "Running");
  const attention = apps.filter((a) => a.status === "Needs attention");
  const unavailable = devices.filter((d) => d.live && !d.online).length;
  const detail = devices.find((d) => d.id === detailId);
  const shownDevices = devices.filter(
    (d) =>
      (filter === "all" || (filter === "online" ? d.online : !d.online)) &&
      (d.name + " " + d.model).toLowerCase().includes(search.toLowerCase()),
  );
  const shownApps = apps.filter(
    (a) =>
      (deviceFilter === "all" || a.deviceId === deviceFilter) &&
      (a.name + " " + a.version).toLowerCase().includes(search.toLowerCase()),
  );
  const shownLogs = logs.filter(
    (l) =>
      l.deviceId === logDevice &&
      (logApp === "all" || l.app === logApp) &&
      meetsLogLevel(l.level, severity) &&
      (l.message + " " + l.app).toLowerCase().includes(logSearch.toLowerCase()),
  );
  function appMenu(a: App) {
    return (
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon"
            aria-label={"Actions for " + a.name}
            disabled={busy === a.id}
          >
            {busy === a.id ? (
              <Loader2 className="animate-spin" />
            ) : (
              <MoreHorizontal />
            )}
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => openLogs(a.deviceId, a.name)}>
            <FileText />
            View logs
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          {a.status !== "Running" ? (
            <DropdownMenuItem
              disabled={!devices.find((d) => d.id === a.deviceId)?.online}
              onSelect={() => void appAction(a, "start")}
            >
              <Play />
              Start application
            </DropdownMenuItem>
          ) : (
            <>
              <DropdownMenuItem
                onSelect={() => setConfirm({ app: a, action: "restart" })}
              >
                <RefreshCw />
                Restart
              </DropdownMenuItem>
              <DropdownMenuItem
                onSelect={() => setConfirm({ app: a, action: "stop" })}
              >
                <Square />
                Stop application
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
    );
  }
  return (
    <SidebarProvider
      style={{ "--sidebar-width": "232px" } as React.CSSProperties}
    >
      <Sidebar>
        <SidebarHeader className="p-0">
          <Brand />
        </SidebarHeader>
        <SidebarContent className="px-4">
          <div className="eyebrow nav-label">Workspace</div>
          <SidebarMenu>
            {nav.map((n) => (
              <SidebarMenuItem key={n.name}>
                <SidebarMenuButton
                  className="nav-item"
                  isActive={view === n.name}
                  onClick={() => navigate(n.name)}
                >
                  <n.icon size={18} />
                  {n.name}
                </SidebarMenuButton>
              </SidebarMenuItem>
            ))}
          </SidebarMenu>
          <div className="sidebar-divider" />
          <div className="eyebrow nav-label">
            {cloudActive ? "Selected cloud device" : "Connected devices"}
          </div>
          {cloudActive ? (
            <div className="sidebar-device">
              <Cpu size={15} />
              {cloudActive.name || "Unnamed device"}
            </div>
          ) : online.length ? (
            online.map((d) => (
              <button
                className="sidebar-device"
                key={d.id}
                onClick={() => setDetailId(d.id)}
              >
                <span className="status-dot" />
                {d.name}
                <ChevronRight size={13} />
              </button>
            ))
          ) : (
            <p className="sidebar-empty">No active connections</p>
          )}
        </SidebarContent>
        <SidebarFooter className="sidebar-footer">
          <Button
            variant="ghost"
            className="justify-start"
            onClick={() => setHelpOpen(true)}
          >
            <CircleHelp />
            Browser guide
            <ArrowUpRight className="ml-auto" />
          </Button>
          <div className="secure">
            <ShieldCheck size={15} />
            Keys stay in your browser
          </div>
          <div className="profile account-profile">
            <div className="profile-avatar">
              <ShieldCheck size={18} />
            </div>
            <div className="account-labels">
              <strong title={auth?.email}>
                {auth ? auth.email || "Signed in" : "Not signed in"}
              </strong>
              <span title={auth?.organization}>
                {auth
                  ? auth.organization || "Organization unavailable"
                  : "Wendy Cloud"}
              </span>
            </div>
            {auth && (
              <Button
                variant="ghost"
                size="icon"
                aria-label="Sign out of Wendy"
                title="Sign out"
                onClick={() => login.current?.signOut()}
              >
                <LogOut size={16} />
              </Button>
            )}
          </div>
          {auth?.profileWarning && (
            <details className="account-warning">
              <summary>Account details unavailable</summary>
              <p>{auth.profileWarning}</p>
            </details>
          )}
        </SidebarFooter>
      </Sidebar>
      <div className="workspace">
        <header className="topbar">
          <div className="topbar-path">
            <SidebarTrigger className="md:hidden" />
            <span>Workspace</span>
            <ChevronRight size={14} />
            <strong>{view}</strong>
          </div>
          <div className="actions">
            <button
              className="search-trigger"
              onClick={() => setCommandOpen(true)}
            >
              <Search size={15} />
              <span>Quick actions</span>
              <kbd>⌘ K</kbd>
            </button>
            <span className="tag live">
              {auth ? "Signed in" : "Signed out"}
            </span>
          </div>
        </header>
        <main className="main-content">
          <WendyLogin
            ref={login}
            onActive={setCloudActive}
            requestedView={view}
            session={auth}
            onSession={setAuth}
            onSignOut={() => {
              clients.current.forEach((c) => c.dispose());
              clients.current.clear();
              setDevices([]);
              setApps([]);
              setLogs([]);
              setActivity([]);
              setAuth(null);
            }}
          />
          {!cloudActive && (
            <>
              <div className="page-heading">
                <div>
                  <h1>{view}</h1>
                  <p className="subtle">{descriptions[view]}</p>
                </div>
                <div className="actions">
                  {devices.length > 0 && (
                    <Button
                      variant="outline"
                      aria-label="Refresh devices"
                      disabled={refreshing}
                      onClick={async () => {
                        setRefreshing(true);
                        await Promise.all(devices.map((d) => refresh(d.id)));
                        setRefreshing(false);
                      }}
                    >
                      <RefreshCw className={refreshing ? "animate-spin" : ""} />
                      Refresh
                    </Button>
                  )}
                  {view === "Applications" && (
                    <Button
                      onClick={() => {
                        setCreateError("");
                        setCreateOpen(true);
                      }}
                      disabled={!online.length}
                    >
                      <Plus />
                      Add application
                    </Button>
                  )}
                </div>
              </div>
              {view === "Overview" && (
                <>
                  <section
                    className={
                      "fleet-summary " +
                      (attention.length ? "has-attention" : "")
                    }
                  >
                    <div className="fleet-summary-copy">
                      <div className="eyebrow" style={{ color: "#bbd3c2" }}>
                        WORKSPACE AT A GLANCE
                      </div>
                      <h2>
                        {unavailable
                          ? "A device connection needs attention."
                          : attention.length
                            ? "An application needs your attention."
                            : online.length
                              ? "Everything is running smoothly."
                              : "Your cloud workspace is ready."}
                      </h2>
                      <p className="subtle">
                        {unavailable
                          ? "Open device details to inspect the connection. The client will keep trying to reconnect."
                          : attention.length
                            ? "Open Applications to inspect the app and its logs."
                            : online.length
                              ? `${online.length === 2 ? "Two" : online.length} ${online.length === 1 ? "device is" : "devices are"} connected. ${running.length ? `${running.length} applications are running.` : "Your workspace is ready."}`
                              : "Sign in to Wendy Cloud to find your devices."}
                      </p>
                      <div className="summary-numbers">
                        <button onClick={() => navigate("Devices")}>
                          <strong>
                            {String(online.length).padStart(2, "0")}
                            <span>
                              {" "}
                              / {String(devices.length).padStart(2, "0")}
                            </span>
                          </strong>
                          <span>Devices online</span>
                        </button>
                        <button onClick={() => navigate("Applications")}>
                          <strong>
                            {String(running.length).padStart(2, "0")}
                          </strong>
                          <span>Running apps</span>
                        </button>
                        <button onClick={() => navigate("Applications")}>
                          <strong>{attention.length}</strong>
                          <span>Apps needing attention</span>
                        </button>
                      </div>
                    </div>
                    <FleetSignal devices={devices} />
                  </section>
                  <div className="section-heading">
                    <h2>
                      Your devices{" "}
                      <span className="subtle">
                        {" "}
                        / {String(devices.length).padStart(2, "0")}
                      </span>
                    </h2>
                    <Button variant="ghost" onClick={() => navigate("Devices")}>
                      View all
                      <ArrowUpRight />
                    </Button>
                  </div>
                  {devices.length ? (
                    <div className="device-grid">
                      {devices.slice(0, 3).map((d) => (
                        <DeviceCard
                          key={d.id}
                          device={d}
                          apps={
                            apps.filter(
                              (a) =>
                                a.deviceId === d.id && a.status === "Running",
                            ).length
                          }
                          onClick={() => setDetailId(d.id)}
                        />
                      ))}
                    </div>
                  ) : (
                    <Empty icon={Cpu} title="Your devices will appear here">
                      <p>
                        Sign in above to discover devices in your cloud
                        organization.
                      </p>
                    </Empty>
                  )}
                  <div className="lower-grid">
                    <section className="panel">
                      <div className="panel-heading">
                        <h2>
                          Applications{" "}
                          <span className="subtle"> / {apps.length}</span>
                        </h2>
                        <Button
                          variant="ghost"
                          onClick={() => navigate("Applications")}
                        >
                          Manage
                          <ArrowUpRight />
                        </Button>
                      </div>
                      {apps.length ? (
                        apps.slice(0, 4).map((a) => (
                          <div className="app-row" key={a.id}>
                            <div className="app-icon">
                              <Box size={18} />
                            </div>
                            <button
                              className="app-info text-left"
                              onClick={() => openLogs(a.deviceId, a.name)}
                            >
                              <strong>{a.name}</strong>
                              <span className="subtle">
                                {devices.find((d) => d.id === a.deviceId)?.name}
                                <span className="divider">·</span>
                                {a.version}
                              </span>
                            </button>
                            <Status value={a.status} />
                            {appMenu(a)}
                          </div>
                        ))
                      ) : (
                        <Empty title="No applications yet">
                          <p>Add a container image or deploy from the CLI.</p>
                        </Empty>
                      )}
                    </section>
                    <section className="panel">
                      <div className="panel-heading">
                        <h2>Recent activity</h2>
                        <span className="subtle">This session</span>
                      </div>
                      {activity.length ? (
                        activity.slice(0, 4).map((a, i) => (
                          <div className="activity" key={a.id}>
                            {i === 1 ? <Wifi size={16} /> : <Check size={16} />}
                            <div>
                              <p>{a.message}</p>
                              <span className="subtle">
                                {a.detail}
                                <span className="divider">·</span>
                                {relativeTime(a.time)}
                              </span>
                            </div>
                          </div>
                        ))
                      ) : (
                        <Empty icon={Activity} title="A fresh session">
                          <p>Device connections and app actions appear here.</p>
                        </Empty>
                      )}
                    </section>
                  </div>
                </>
              )}
              {view === "Devices" && (
                <>
                  <div className="toolbar">
                    <Tabs value={filter} onValueChange={setFilter}>
                      <TabsList>
                        <TabsTrigger value="all">
                          All devices{" "}
                          <span className="tab-count">{devices.length}</span>
                        </TabsTrigger>
                        <TabsTrigger value="online">Connected</TabsTrigger>
                        <TabsTrigger value="offline">Offline</TabsTrigger>
                      </TabsList>
                    </Tabs>
                    <div className="search-field">
                      <Search size={16} />
                      <Input
                        aria-label="Search devices"
                        placeholder="Find a device…"
                        value={search}
                        onChange={(e) => setSearch(e.target.value)}
                      />
                    </div>
                  </div>
                  <div className="device-grid">
                    {shownDevices.map((d) => (
                      <DeviceCard
                        key={d.id}
                        device={d}
                        apps={
                          apps.filter(
                            (a) =>
                              a.deviceId === d.id && a.status === "Running",
                          ).length
                        }
                        onClick={() => setDetailId(d.id)}
                      />
                    ))}
                  </div>
                  {!shownDevices.length && (
                    <Empty icon={Search} title="No matching devices">
                      <p>Try a different search or connect a device.</p>
                      <Button
                        variant="outline"
                        onClick={() => {
                          setSearch("");
                          setFilter("all");
                        }}
                      >
                        Clear filters
                      </Button>
                    </Empty>
                  )}
                  <div className="capability-strip">
                    <Network size={21} />
                    <div>
                      <h3>Bring your own device.</h3>
                      <p>
                        Jetson, Raspberry Pi, or any device running the Wendy
                        agent.
                      </p>
                    </div>
                  </div>
                </>
              )}
              {view === "Applications" && (
                <>
                  <div className="toolbar">
                    <div className="actions">
                      <Pick
                        value={deviceFilter}
                        onChange={setDeviceFilter}
                        label="Filter by device"
                        items={[{ id: "all", name: "All devices" }, ...devices]}
                      />
                      <span className="subtle">
                        {running.length} running ·{" "}
                        {apps.length - running.length} stopped or unhealthy
                      </span>
                    </div>
                    <div className="search-field">
                      <Search size={16} />
                      <Input
                        aria-label="Search applications"
                        placeholder="Find an application…"
                        value={search}
                        onChange={(e) => setSearch(e.target.value)}
                      />
                    </div>
                  </div>
                  <div className="panel">
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Application</TableHead>
                          <TableHead>Device</TableHead>
                          <TableHead>Status</TableHead>
                          <TableHead>Version</TableHead>
                          <TableHead className="text-right">Actions</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {shownApps.map((a) => (
                          <TableRow key={a.id}>
                            <TableCell>
                              <button
                                className="table-app"
                                onClick={() => openLogs(a.deviceId, a.name)}
                              >
                                <span className="app-icon">
                                  <Box size={18} />
                                </span>
                                <span>{a.name}</span>
                              </button>
                            </TableCell>
                            <TableCell>
                              {devices.find((d) => d.id === a.deviceId)?.name}
                            </TableCell>
                            <TableCell>
                              <Status value={a.status} />
                            </TableCell>
                            <TableCell className="mono muted">
                              {a.version}
                            </TableCell>
                            <TableCell>
                              <div className="actions justify-end">
                                <Button
                                  variant="ghost"
                                  size="sm"
                                  onClick={() => openLogs(a.deviceId, a.name)}
                                >
                                  Logs
                                </Button>
                                {a.status !== "Running" && (
                                  <Button
                                    variant="outline"
                                    size="sm"
                                    disabled={
                                      !!busy ||
                                      !devices.find((d) => d.id === a.deviceId)
                                        ?.online
                                    }
                                    onClick={() => void appAction(a, "start")}
                                  >
                                    {busy === a.id ? (
                                      <Loader2 className="animate-spin" />
                                    ) : (
                                      <Play />
                                    )}
                                    Start
                                  </Button>
                                )}
                                {appMenu(a)}
                              </div>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                    {!shownApps.length && (
                      <Empty title="No matching applications">
                        <p>
                          Clear your filters or add your first container image.
                        </p>
                        <Button
                          variant="outline"
                          onClick={() => {
                            setSearch("");
                            setDeviceFilter("all");
                          }}
                        >
                          Clear filters
                        </Button>
                      </Empty>
                    )}
                  </div>
                  <div className="capability-strip">
                    <Terminal size={21} />
                    <div>
                      <h3>Working from source?</h3>
                      <p>
                        Build and deploy your local project with the Wendy CLI.
                      </p>
                    </div>
                    <button
                      className="code-copy"
                      onClick={() =>
                        void navigator.clipboard
                          .writeText("wendy run --device <name>")
                          .then(() => toast.success("Command copied"))
                          .catch(() => toast.error("Clipboard unavailable"))
                      }
                    >
                      <code>wendy run --device &lt;name&gt;</code>
                      <FileText size={14} />
                    </button>
                  </div>
                </>
              )}
              {view === "Logs" && (
                <>
                  <div className="toolbar">
                    <div className="actions">
                      <Pick
                        value={logDevice}
                        onChange={(v) => {
                          setLogDevice(v);
                          setLogApp("all");
                          setLogError("");
                        }}
                        label="Log device"
                        items={devices}
                      />
                      <Pick
                        value={logApp}
                        onChange={setLogApp}
                        label="Log application"
                        items={[
                          { id: "all", name: "All applications" },
                          ...apps
                            .filter((a) => a.deviceId === logDevice)
                            .map((a) => ({ id: a.name, name: a.name })),
                        ]}
                      />
                      <Pick
                        value={severity}
                        onChange={setSeverity}
                        label="Log severity"
                        items={[
                          { id: "all", name: "All levels" },
                          { id: "INFO", name: "Info & above" },
                          { id: "WARN", name: "Warning & above" },
                          { id: "ERROR", name: "Errors only" },
                        ]}
                      />
                    </div>
                    <div className="actions">
                      <Button
                        variant="outline"
                        onClick={() => setPaused((v) => !v)}
                      >
                        {paused ? <Play /> : <Pause />}
                        {paused ? "Resume" : "Pause"}
                      </Button>
                      <Button
                        variant="outline"
                        aria-label="Download filtered logs"
                        onClick={() =>
                          downloadText(
                            "wendy-logs.txt",
                            shownLogs
                              .map(
                                (l) =>
                                  `${l.time} ${l.level} [${l.app}] ${l.message}`,
                              )
                              .join("\n"),
                          )
                        }
                      >
                        <ArrowDownToLine />
                      </Button>
                    </div>
                  </div>
                  <section className="panel log-panel">
                    <div className="log-search">
                      <Search size={16} />
                      <Input
                        aria-label="Search log messages"
                        value={logSearch}
                        onChange={(e) => setLogSearch(e.target.value)}
                        placeholder="Search logs…"
                      />
                      <span className={"tag " + (!paused ? "live" : "")}>
                        {paused
                          ? "Paused"
                          : clients.current.has(logDevice)
                            ? "Following"
                            : "Disconnected"}
                      </span>
                    </div>
                    {logError && (
                      <div className="inline-error" role="alert">
                        {logError}
                      </div>
                    )}
                    <div
                      className="log-lines"
                      role="log"
                      aria-label="Application logs"
                    >
                      {shownLogs.length ? (
                        shownLogs.map((l) => (
                          <div className="log-line" key={l.id}>
                            <time>
                              {new Date(l.time).toLocaleTimeString("en-GB")}
                            </time>
                            <span
                              className={"log-level " + l.level.toLowerCase()}
                            >
                              {l.level}
                            </span>
                            <span className="log-app">{l.app}</span>
                            <span className="log-message">{l.message}</span>
                          </div>
                        ))
                      ) : (
                        <Empty
                          icon={FileText}
                          title={
                            logSearch || severity !== "all"
                              ? "No matching log messages"
                              : "Waiting for logs"
                          }
                        >
                          <p>
                            {devices.find((d) => d.id === logDevice)?.online
                              ? "New messages will appear here."
                              : "Select a connected device to follow its logs."}
                          </p>
                        </Empty>
                      )}
                      <div ref={logEnd} />
                    </div>
                    <div className="log-foot">
                      <span>
                        {shownLogs.length} messages · Keeps the latest 500
                      </span>
                      <span>
                        {paused ? "Stream paused" : "Following new messages"}
                      </span>
                    </div>
                  </section>
                </>
              )}
            </>
          )}
          <footer className="bottom-note">
            <span>Device metrics refresh every 5 seconds when connected.</span>
            <button onClick={() => setHelpOpen(true)}>
              Wendy Client / Browser edition
              <ArrowUpRight size={12} />
            </button>
          </footer>
        </main>
      </div>
      <CommandDialog
        open={commandOpen}
        onOpenChange={setCommandOpen}
        title="Quick actions"
        description="Find a workspace view or device."
      >
        <CommandInput placeholder="Where do you want to go?" />
        <CommandList>
          <CommandEmpty>No results found.</CommandEmpty>
          <CommandGroup heading="Workspace">
            {nav.map((n) => (
              <CommandItem key={n.name} onSelect={() => navigate(n.name)}>
                <n.icon />
                {n.name}
              </CommandItem>
            ))}
          </CommandGroup>
          <CommandGroup heading="Devices">
            {devices.map((d) => (
              <CommandItem
                key={d.id}
                onSelect={() => {
                  setCommandOpen(false);
                  setDetailId(d.id);
                }}
              >
                <Cpu />
                {d.name}
                <span className="ml-auto subtle">
                  {d.online ? "Connected" : "Offline"}
                </span>
              </CommandItem>
            ))}
          </CommandGroup>
        </CommandList>
      </CommandDialog>
      <Sheet
        open={!!detail}
        onOpenChange={(v) => {
          if (!v) setDetailId(null);
        }}
      >
        <SheetContent className="device-sheet">
          {detail && (
            <>
              <SheetHeader>
                <div className="device-icon mb-3">
                  <Cpu size={24} />
                </div>
                <SheetTitle className="text-2xl">{detail.name}</SheetTitle>
                <SheetDescription>{detail.model}</SheetDescription>
                <div className="mt-3">
                  <Status value={detail.online ? "Connected" : "Offline"} />
                </div>
              </SheetHeader>
              <div className="sheet-body">
                {deviceErrors[detail.id] && (
                  <div className="inline-error">{deviceErrors[detail.id]}</div>
                )}
                <div className="detail-metrics">
                  <div>
                    <span>CPU usage</span>
                    <strong>
                      {detail.cpu === null ? "—" : Math.round(detail.cpu) + "%"}
                    </strong>
                    <Sparkline values={detail.history} />
                  </div>
                  <div>
                    <span>Memory</span>
                    <strong>{formatBytes(detail.memory)}</strong>
                    <Progress
                      value={
                        detail.memory && detail.memoryTotal
                          ? (detail.memory / detail.memoryTotal) * 100
                          : 0
                      }
                    />
                    <small>of {formatBytes(detail.memoryTotal)}</small>
                  </div>
                </div>
                <div className="detail-list">
                  <div>
                    <span>Agent version</span>
                    <strong>{detail.version || "—"}</strong>
                  </div>
                  <div>
                    <span>Operating system</span>
                    <strong>{detail.os}</strong>
                  </div>
                  <div>
                    <span>Architecture</span>
                    <strong>{detail.arch}</strong>
                  </div>
                  <div>
                    <span>Storage</span>
                    <strong>
                      {formatBytes(detail.diskUsed)} /{" "}
                      {formatBytes(detail.diskTotal)}
                    </strong>
                  </div>
                  <div>
                    <span>Temperature</span>
                    <strong>
                      {detail.temperature === null
                        ? "—"
                        : detail.temperature + "°C"}
                    </strong>
                  </div>
                  <div>
                    <span>Connection</span>
                    <strong>
                      {detail.live ? "WebSocket · mTLS" : "Disconnected"}
                    </strong>
                  </div>
                </div>
                <div className="section-heading mt-8">
                  <h2>Applications</h2>
                  <span className="subtle">
                    {apps.filter((a) => a.deviceId === detail.id).length}
                  </span>
                </div>
                {apps
                  .filter((a) => a.deviceId === detail.id)
                  .map((a) => (
                    <div className="detail-app" key={a.id}>
                      <Box size={16} />
                      <button onClick={() => openLogs(a.deviceId, a.name)}>
                        {a.name}
                      </button>
                      <Status value={a.status} />
                    </div>
                  ))}
                <div className="detail-actions">
                  <Button
                    variant="outline"
                    disabled={!detail.online}
                    onClick={() => openLogs(detail.id)}
                  >
                    <FileText />
                    View logs
                  </Button>
                </div>
                {detail.live ? (
                  <Button
                    variant="ghost"
                    className="w-full muted"
                    onClick={() => disconnect(detail)}
                  >
                    <Unplug />
                    Disconnect device
                  </Button>
                ) : (
                  <p className="field-note">
                    Connect to this device to use its controls.
                  </p>
                )}
              </div>
            </>
          )}
        </SheetContent>
      </Sheet>
      <AlertDialog
        open={!!confirm}
        onOpenChange={(v) => {
          if (!v && !busy) setConfirm(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {confirm?.action === "stop" ? "Stop" : "Restart"}{" "}
              {confirm?.app.name}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.action === "stop"
                ? "The application will stop serving requests until you start it again."
                : "The application will be briefly unavailable while it restarts."}{" "}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={!!busy}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={!!busy}
              onClick={(e) => {
                e.preventDefault();
                if (confirm) void appAction(confirm.app, confirm.action);
              }}
            >
              {busy ? (
                <Loader2 className="animate-spin" />
              ) : confirm?.action === "stop" ? (
                "Stop application"
              ) : (
                "Restart application"
              )}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <Dialog
        open={createOpen}
        onOpenChange={(v) => {
          if (!busy) {
            setCreateOpen(v);
            if (!v) setNewEnv("");
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="text-2xl">Add an application</DialogTitle>
            <DialogDescription>
              Create an app from a container image. You can start it after
              creation.
            </DialogDescription>
          </DialogHeader>
          <form className="form-stack" onSubmit={createApp}>
            <div>
              <Label>Device</Label>
              <Pick
                value={newDevice}
                onChange={setNewDevice}
                label="Deployment device"
                items={online}
              />
            </div>
            <div>
              <Label htmlFor="app-name">Application name</Label>
              <Input
                id="app-name"
                value={newApp}
                onChange={(e) => setNewApp(e.target.value)}
                placeholder="my-edge-app"
                required
                maxLength={80}
              />
            </div>
            <div>
              <Label htmlFor="app-image">Container image</Label>
              <Input
                id="app-image"
                value={newImage}
                onChange={(e) => setNewImage(e.target.value)}
                placeholder="ghcr.io/your-team/app:latest"
                required
              />
              <p className="field-note">
                The image must support the device architecture and be accessible
                to its agent.
              </p>
            </div>
            <div>
              <Label htmlFor="app-env">
                Environment variables <span className="muted">· optional</span>
              </Label>
              <Textarea
                id="app-env"
                value={newEnv}
                onChange={(e) => setNewEnv(e.target.value)}
                placeholder={"LOG_LEVEL=info\nPORT=8080"}
                spellCheck={false}
                className="mono"
              />
            </div>
            {createError && (
              <div className="inline-error" role="alert">
                {createError}
              </div>
            )}
            <div className="dialog-actions">
              <Button
                variant="ghost"
                type="button"
                disabled={!!busy}
                onClick={() => setCreateOpen(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={!!busy}>
                {busy === "create" ? (
                  <Loader2 className="animate-spin" />
                ) : (
                  <Plus />
                )}
                Create application
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
      <Dialog open={helpOpen} onOpenChange={setHelpOpen}>
        <DialogContent className="guide-dialog">
          <DialogHeader>
            <DialogTitle className="text-2xl">
              Your browser, at the edge.
            </DialogTitle>
            <DialogDescription>
              What you can do with Wendy Client.
            </DialogDescription>
          </DialogHeader>
          <div className="guide-list">
            {[
              {
                icon: Cpu,
                title: "Manage your devices",
                text: "Sign in with Wendy to view the devices registered to your organization.",
              },
              {
                icon: Box,
                title: "Control applications",
                text: "Select an online cloud device to inspect and control its applications.",
              },
              {
                icon: Activity,
                title: "Inspect cloud telemetry",
                text: "Select an online cloud device for its live dashboard, OTel logs, metrics, and traces.",
              },
            ].map((x) => (
              <div key={x.title}>
                <x.icon size={20} />
                <section>
                  <h3>{x.title}</h3>
                  <p>{x.text}</p>
                </section>
              </div>
            ))}
          </div>
          <div className="guide-limit">
            <h3>Keep the CLI for local hardware tasks.</h3>
            <p>
              Building local projects, flashing disks, USB discovery, and local
              network discovery need the native CLI. This browser client
              connects through a WebSocket relay.
            </p>
          </div>
        </DialogContent>
      </Dialog>
      <Toaster theme="dark" position="bottom-right" richColors closeButton />
    </SidebarProvider>
  );
}

function parseLogs(data: unknown, deviceId: string): Log[] {
  type Value = {
    stringValue?: string;
    intValue?: string;
    doubleValue?: number;
    boolValue?: boolean;
  };
  type Record = {
    timeUnixNano?: string;
    observedTimeUnixNano?: string;
    severityText?: string;
    severityNumber?: number;
    body?: Value;
    attributes?: { key: string; value: Value }[];
  };
  type Resource = {
    resource?: { attributes?: { key: string; value: Value }[] };
    scopeLogs?: { logRecords?: Record[] }[];
  };
  const resources =
    (data as { logs?: { resourceLogs?: Resource[] } })?.logs?.resourceLogs ||
    [];
  return resources.flatMap((r) => {
    const attributes = r.resource?.attributes || [];
    const app =
      attributes.find((a) => a.key === "service.name")?.value.stringValue ||
      attributes.find((a) => a.key === "wendy.app.name")?.value.stringValue ||
      "device";
    return (r.scopeLogs || []).flatMap((s) =>
      (s.logRecords || []).map((l) => {
        const nano = l.timeUnixNano || l.observedTimeUnixNano;
        const millis = nano
          ? Number(BigInt(nano) / BigInt(1000000))
          : Date.now();
        const message = l.body?.stringValue ?? JSON.stringify(l.body || {});
        return {
          id: deviceId + ":" + nano + ":" + app + ":" + message,
          time: new Date(millis).toISOString(),
          level: logSeverity(l).toUpperCase(),
          app,
          deviceId,
          message,
        };
      }),
    );
  });
}
