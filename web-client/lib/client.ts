export type View =
  "Overview" | "Devices" | "Applications" | "Logs" | "Metrics" | "Traces";
export type App = {
  id: string;
  deviceId: string;
  name: string;
  version: string;
  status: "Running" | "Stopped" | "Needs attention";
  failures: number;
  port?: number;
};
export type Device = {
  id: string;
  name: string;
  model: string;
  online: boolean;
  live?: boolean;
  cpu: number | null;
  memory: number | null;
  memoryTotal: number | null;
  diskUsed: number | null;
  diskTotal: number | null;
  version: string;
  os: string;
  arch: string;
  temperature: number | null;
  history: number[];
  updated: string;
  relay?: string;
};
export type Log = {
  id: string;
  time: string;
  level: string;
  app: string;
  deviceId: string;
  message: string;
};
export type ActivityEntry = {
  id: string;
  message: string;
  detail: string;
  time: string;
};
export type RawSnapshot = {
  version: {
    version?: string;
    os?: string;
    osVersion?: string;
    cpuArchitecture?: string;
    deviceType?: string;
    diskUsedBytes?: string;
    diskTotalBytes?: string;
  };
  apps: {
    appName: string;
    appVersion?: string;
    runningState?: string;
    failureCount?: number;
    httpPort?: number;
  }[];
  stats?: {
    host?: {
      cpuTotalJiffies?: string;
      cpuIdleJiffies?: string;
      memTotalBytes?: string;
      memAvailableBytes?: string;
      thermalZones?: { tempC?: number }[];
    };
  };
  warnings: string[];
};
export type ClientEvent = { event: string; data: unknown };
export class WendyClient {
  private worker: Worker;
  private next = 0;
  private pending = new Map<
    number,
    {
      resolve: (v: unknown) => void;
      reject: (e: Error) => void;
      timer: ReturnType<typeof setTimeout>;
    }
  >();
  private listeners = new Set<(e: ClientEvent) => void>();
  private ready: Promise<void>;
  private disposed = false;
  private cpuPrevious?: { total: number; idle: number };
  constructor() {
    this.worker = new Worker("/wendy-worker.js");
    this.ready = new Promise((resolve, reject) => {
      const timer = setTimeout(
        () =>
          reject(
            new Error(
              "The browser client took too long to load. Reload and try again.",
            ),
          ),
        45000,
      );
      this.worker.onmessage = ({ data }) => {
        if (data.ready) {
          clearTimeout(timer);
          resolve();
          return;
        }
        if (data.fatal) {
          clearTimeout(timer);
          reject(new Error(data.fatal));
          this.fail(new Error(data.fatal));
          return;
        }
        if (data.event) {
          this.listeners.forEach((fn) => fn(data));
          return;
        }
        const call = this.pending.get(data.id);
        if (!call) return;
        clearTimeout(call.timer);
        this.pending.delete(data.id);
        if (data.error) call.reject(new Error(data.error));
        else call.resolve(data.result);
      };
      this.worker.onerror = () => {
        clearTimeout(timer);
        const e = new Error(
          "The browser client could not start. Check that WebAssembly is supported.",
        );
        reject(e);
        this.fail(e);
      };
    });
  }
  async call<T = unknown>(method: string, params: unknown = {}): Promise<T> {
    await this.ready;
    if (this.disposed) throw new Error("Connection closed");
    const id = ++this.next;
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error("The device did not respond in time."));
      }, 40000);
      this.pending.set(id, { resolve: (v) => resolve(v as T), reject, timer });
      this.worker.postMessage({ id, method, params });
    });
  }
  onEvent(fn: (e: ClientEvent) => void) {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  }
  private fail(error: Error) {
    for (const p of this.pending.values()) {
      clearTimeout(p.timer);
      p.reject(error);
    }
    this.pending.clear();
  }
  dispose() {
    this.disposed = true;
    this.worker.terminate();
    this.fail(new Error("Connection closed"));
    this.listeners.clear();
  }
  async snapshot(
    previous: Device,
  ): Promise<{ device: Device; apps: App[]; warnings: string[] }> {
    const data = await this.call<RawSnapshot>("snapshot");
    const h = data.stats?.host;
    let cpu: number | null = null;
    if (h?.cpuTotalJiffies) {
      const current = {
        total: Number(h.cpuTotalJiffies),
        idle: Number(h.cpuIdleJiffies || 0),
      };
      if (this.cpuPrevious && current.total > this.cpuPrevious.total) {
        cpu = Math.max(
          0,
          Math.min(
            100,
            100 *
              (1 -
                (current.idle - this.cpuPrevious.idle) /
                  (current.total - this.cpuPrevious.total)),
          ),
        );
      }
      this.cpuPrevious = current;
    }
    const total = h?.memTotalBytes ? Number(h.memTotalBytes) : null;
    return {
      device: {
        ...previous,
        online: true,
        cpu,
        memoryTotal: total,
        memory:
          total === null ? null : total - Number(h?.memAvailableBytes || 0),
        diskUsed: data.version.diskUsedBytes
          ? Number(data.version.diskUsedBytes)
          : null,
        diskTotal: data.version.diskTotalBytes
          ? Number(data.version.diskTotalBytes)
          : null,
        version: data.version.version || "Unknown",
        model: data.version.deviceType || "Wendy device",
        os: data.version.osVersion || data.version.os || "WendyOS",
        arch: data.version.cpuArchitecture || "Unknown",
        temperature: h?.thermalZones?.[0]?.tempC ?? null,
        history:
          cpu === null
            ? previous.history
            : [...previous.history, cpu].slice(-30),
        updated: new Date().toISOString(),
      },
      apps: data.apps.map((a) => ({
        id: previous.id + ":" + a.appName,
        deviceId: previous.id,
        name: a.appName,
        version: a.appVersion || "—",
        status:
          a.runningState === "RUNNING"
            ? "Running"
            : a.runningState === "CRASH_LOOPING"
              ? "Needs attention"
              : "Stopped",
        failures: a.failureCount || 0,
        port: a.httpPort,
      })),
      warnings: data.warnings || [],
    };
  }
}

export function formatBytes(n: number | null) {
  if (n === null) return "—";
  if (n >= 1024 ** 3) return (n / 1024 ** 3).toFixed(1) + " GB";
  return Math.round(n / 1024 ** 2) + " MB";
}
export function relativeTime(iso: string) {
  const s = Math.max(
    0,
    Math.floor((Date.now() - new Date(iso).getTime()) / 1000),
  );
  return s < 60
    ? "Just now"
    : s < 3600
      ? `${Math.floor(s / 60)} min ago`
      : `${Math.floor(s / 3600)} hr ago`;
}
export function downloadText(name: string, text: string, type = "text/plain") {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
