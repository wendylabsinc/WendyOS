export type DeviceLogLevel = "all" | "trace" | "debug" | "info" | "warn" | "error" | "fatal";

export type DeviceLogSource =
  | { kind: "container"; runtime: "docker" | "apple-container"; id: string }
  | { kind: "simulator"; id: string };

export type DeviceLogsStartOptions = {
  target: string;
  app?: string;
  level?: DeviceLogLevel;
  source?: DeviceLogSource;
};

export type DeviceLogEntry = {
  id: number;
  timestamp: string;
  severity: string;
  service: string;
  body: string;
  attributes: Record<string, string>;
};

export type DeviceLogSnapshot = {
  id: string;
  target: string;
  app: string;
  level: DeviceLogLevel;
  running: boolean;
  entries: DeviceLogEntry[];
  dropped: number;
  error?: string;
  exitCode?: number;
};

export interface DeviceLogsAPI {
  deviceLogsStart(args: DeviceLogsStartOptions): Promise<{ id: string }>;
  deviceLogsSnapshot(id: string): Promise<DeviceLogSnapshot>;
  deviceLogsStop(id: string): Promise<void>;
}
