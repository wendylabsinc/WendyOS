import type { DeviceLogsAPI } from "./log-types";

export type Runtime = "docker" | "apple-container";
export type SimulatorKind = "go2" | "g1" | "raspberry-pi";
export type VirtualMachine = {
  name: string;
  state: string;
  address?: string;
  version?: string;
};
export type Session = {
  id: string;
  title: string;
  running: boolean;
  exitCode?: number;
  sequence: number;
};
export type SessionEvent = Session & { type: "data" | "exit"; data?: string };
export type Simulator = {
  id: string;
  name: string;
  kind: SimulatorKind;
  container?: string;
  runtime: Runtime | "qemu";
  url?: string;
  target?: string;
  address?: string;
  reachable: boolean;
  ready: boolean;
  healthy: boolean;
  mode?: string;
  error?: string;
};
export type RuntimeStatus = { id: Runtime; ready: boolean; detail: string };
export type HostStatus = {
  version: string;
  runtimes: RuntimeStatus[];
  platform: string;
  image: string;
};
export type Container = {
  id: string;
  name: string;
  image: string;
  state: string;
  ports: string;
};
export type Drive = {
  id: string;
  name: string;
  capacity: number;
  isExternal: boolean;
};
export type InstallPlan = {
  id: string;
  device_type: string;
  version: string;
  method: string;
  erase_scope: string;
  target?: {
    id: string;
    name: string;
    capacity_bytes: number;
    removable: boolean;
  };
  artifact_sha256: string;
  requirements: string[];
  next_steps: string[];
  command: string[];
};
export type ProjectAction = {
  action: "chat" | "chat-setup" | "build" | "run";
  project: string;
  runtime: Runtime;
  target: string;
};
export type DeviceSnapshot = {
  target: string;
  sampledAt: string;
  host: {
    cpuPercent?: number;
    cpuCount?: number;
    memUsedBytes?: number;
    memTotalBytes?: number;
    containerStorage?: { mountpoint: string; usedBytes: number; totalBytes: number };
    gpus?: { index: number; name: string; utilPercent: number; memUsedBytes?: number; memTotalBytes?: number; tempC?: number; powerW?: number }[];
    thermalZones?: { name: string; tempC: number }[];
    maximumTemperature?: { name: string; tempC: number };
    battery?: { percent: number; state: string; secondsRemaining?: number };
  };
  containers: { name: string; state: string; cpuPercent?: number; memBytes?: number }[];
};
export type DiscoveredDevice = { name: string; target: string; address: string; transport: string; version?: string };
export interface DesktopAPI extends DeviceLogsAPI {
  status(): Promise<HostStatus>;
  containers(runtime: Runtime): Promise<Container[]>;
  virtualMachines(): Promise<VirtualMachine[]>;
  deviceSnapshot(args: { target: string }): Promise<DeviceSnapshot>;
  openProject(): Promise<string | null>;
  projectAction(args: ProjectAction): Promise<Session>;
  startApple(): Promise<Session>;
  simulators(): Promise<Simulator[]>;
  createSimulator(args: {
    name: string;
    runtime: Runtime;
    kind: SimulatorKind;
  }): Promise<{ simulator: Simulator; session: Session }>;
  simulatorAction(args: {
    id: string;
    action: "start" | "stop" | "retry";
  }): Promise<Session>;
  simulatorRequest(args: {
    id: string;
    path: string;
    body?: Record<string, unknown>;
  }): Promise<unknown>;
  drives(): Promise<Drive[]>;
  plan(args: {
    deviceType: string;
    drive: string;
    version: string;
  }): Promise<InstallPlan>;
  flash(id: string): Promise<Session | null>;
  verify(args: {
    address: string;
    planId?: string;
  }): Promise<Record<string, unknown>>;
  discover(): Promise<DiscoveredDevice[]>;
  sessions(): Promise<Session[]>;
  snapshot(id: string): Promise<Session & { output: string }>;
  input(id: string, data: string): Promise<void>;
  resize(id: string, cols: number, rows: number): Promise<void>;
  cancel(id: string): Promise<void>;
  onSession(callback: (event: SessionEvent) => void): () => void;
}
declare global {
  interface Window {
    wendyDesktop?: DesktopAPI;
  }
}
