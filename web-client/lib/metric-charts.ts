import type { MetricSample, TelemetryRow } from "./telemetry";

export type MetricSeries = {
  key: string;
  name: string;
  title: string;
  service: string;
  serviceLabel: string;
  subtitle: string;
  unit: string;
  kind: "gauge" | "counter" | "rate" | "distribution";
  points: { time: number; value: number | null }[];
  latest: number | null;
  note: string;
  detail: unknown;
  samples: MetricSample[];
};
const MAX_SERIES = 100;
const MAX_POINTS = 120;
const WINDOW_MS = 30 * 60 * 1000;
const names: { [key: string]: string } = {
  "process.cpu.time": "CPU time",
  "container.cpu.usage": "CPU time",
  "container.cpu.time": "CPU time",
  "system.cpu.time": "CPU time",
  "process.cpu.utilization": "CPU usage",
  "container.cpu.utilization": "CPU usage",
  "system.cpu.utilization": "CPU usage",
  "process.memory.usage": "Memory usage",
  "process.memory.virtual": "Virtual memory",
  "container.memory.usage": "Memory usage",
  "system.memory.usage": "Memory usage",
  "system.memory.utilization": "Memory usage",
  "process.thread.count": "Threads",
  "process.open_file_descriptor.count": "Open files",
  "system.network.io": "Network traffic",
  "system.disk.io": "Disk traffic",
  "system.filesystem.usage": "Storage usage",
  "system.filesystem.utilization": "Storage usage",
  "http.server.request.duration": "Request duration",
  "http.client.request.duration": "Request duration",
  "rpc.server.duration": "RPC duration",
  "rpc.client.duration": "RPC duration",
};
const attributes: { [key: string]: string } = {
  "cpu.mode": "CPU mode", "cpu.state": "CPU state", "system.cpu.state": "CPU state",
  "cpu.logical_number": "Core", "cpu": "Core", "system.cpu.logical_number": "Core",
  "direction": "Direction", "network.io.direction": "Direction", "state": "State",
  "device": "Device", "network.interface.name": "Interface", "mountpoint": "Mount",
  "http.request.method": "Method", "http.response.status_code": "Status",
  "http.route": "Route", "process.pid": "Process ID",
};
const modes: { [key: string]: string } = { user: "User", system: "System", idle: "Idle", wait: "I/O wait", receive: "Received", transmit: "Sent", used: "Used", free: "Free" };
const friendlyServices: { [key: string]: string } = {
  "wendy-agent": "Wendy agent", "sh.wendy.device-panel": "Device panel",
  "sh.wendy.simulator.go2": "Go2 simulator", "sh.wendy.simulator.g1": "G1 simulator",
};
export function metricTitle(name: string) { return names[name] || name; }
export function metricServiceLabel(service: string) { return friendlyServices[service] || service; }
function subtitle(sample: MetricSample) {
  return Object.entries(sample.attributes).sort(([a], [b]) => a.localeCompare(b)).map(([key, value]) => {
    if (["cpu.mode", "cpu.state", "system.cpu.state"].includes(key)) return modes[value] || value;
    return `${attributes[key] || key}: ${modes[value] || value}`;
  }).join(" · ");
}
function isCPURate(name: string, sample: MetricSample) {
  return ["process.cpu.time", "container.cpu.usage", "container.cpu.time"].includes(name) && sample.unit === "s" && sample.kind === "sum" && sample.monotonic === true && [2, "AGGREGATION_TEMPORALITY_CUMULATIVE"].includes(sample.temporality ?? "");
}
function present(row: TelemetryRow, samples: MetricSample[]): MetricSeries {
  const last = samples[samples.length - 1];
  const rate = isCPURate(row.name, last);
  const ratio = last.unit === "1" && /\.utilization$/.test(row.name) && row.name in names;
  const distribution = last.kind === "histogram" || last.kind === "summary";
  const points = samples.map((sample, index) => {
    let value = sample.value;
    if (rate) {
      const previous = samples[index - 1];
      value = previous && value !== null && previous.value !== null && sample.time > previous.time && value >= previous.value && sample.startTime === previous.startTime
        ? (value - previous.value) / ((sample.time - previous.time) / 1000) * 100 : null;
    } else if (ratio && value !== null) value *= 100;
    return { time: sample.time, value: value !== null && Number.isFinite(value) ? value : null };
  });
  return {
    key: row.key, name: row.name, title: rate ? "CPU usage" : distribution ? `${metricTitle(row.name)} · average` : metricTitle(row.name),
    service: row.service, serviceLabel: metricServiceLabel(row.service), subtitle: subtitle(last),
    unit: rate || ratio ? "%" : last.unit,
    kind: rate ? "rate" : distribution ? "distribution" : last.kind === "sum" ? "counter" : "gauge",
    points, latest: points[points.length - 1]?.value ?? null,
    note: rate ? "100% equals one CPU core. History starts after two samples." : distribution ? "Average of the observations in each reported distribution." : last.kind === "sum" ? "Reported counter value." : "Live samples from this device.",
    detail: row.detail, samples,
  };
}

// Preserve samples, rather than the latest row alone. Rebuild rates after sorting
// so reconnect history, duplicate batches and counter resets cannot create spikes.
export function updateMetricHistory(previous: MetricSeries[], incoming: TelemetryRow[]): MetricSeries[] {
  const map = new Map(previous.map((series) => [series.key, series]));
  for (const row of incoming) {
    if (!row.metric || !Number.isFinite(row.metric.time) || row.metric.time <= 0) continue;
    const old = map.get(row.key);
    const samplesByTime = new Map((old?.samples || []).map((sample) => [sample.time, sample]));
    samplesByTime.set(row.metric.time, row.metric);
    const sorted = [...samplesByTime.values()].sort((a, b) => a.time - b.time);
    const newest = sorted[sorted.length - 1].time;
    const samples = sorted.filter((sample) => sample.time >= newest - WINDOW_MS).slice(-MAX_POINTS);
    // Older backfill must not replace the latest metadata shown in Details.
    const latestRow = old && row.metric.time < old.samples[old.samples.length - 1].time ? { ...row, detail: old.detail } : row;
    map.set(row.key, present(latestRow, samples));
  }
  return [...map.values()].sort((a, b) => b.samples[b.samples.length - 1].time - a.samples[a.samples.length - 1].time).slice(0, MAX_SERIES);
}

export function formatMetricValue(value: number | null | undefined, unit = "") {
  if (value == null || !Number.isFinite(value)) return "—";
  const number = (n: number) => n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  if (["By", "B", "bytes"].includes(unit)) {
    const power = value === 0 ? 0 : Math.min(4, Math.max(0, Math.floor(Math.log10(Math.abs(value)) / 3)));
    return `${number(value / 1000 ** power)} ${["B", "KB", "MB", "GB", "TB"][power]}`;
  }
  if (unit === "%") return `${number(value)}%`;
  if (unit === "s" && value !== 0 && Math.abs(value) < 1) return `${number(value * 1000)} ms`;
  if (unit === "ns") return `${number(value / 1e6)} ms`;
  if (unit === "Cel") return `${number(value)} °C`;
  if (!unit || unit === "1") return number(value);
  return `${number(value)} ${unit}`;
}
