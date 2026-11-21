import type { WendyClient } from "@/lib/client";
import type { Operation } from "./operations";
export type Field = { name: string; kind: string; list: boolean; map: boolean; oneof?: string; options?: string[]; fields?: Field[] };
export type StreamEvent = { stream: string; message?: unknown; done?: boolean; error?: string };
let nextStream = 0;
export function streamID() { return `device-${++nextStream}`; }
export function rpc(client: WendyClient, operation: Operation, message: Record<string, unknown>, stream?: string) {
  return client.call("device-rpc", { service: operation.service, method: operation.method, message, stream });
}
export function label(name: string) {
  const special: Record<string, string> = {
    ssid: "Network name", deviceId: "Device ID", appName: "Application", appId: "Application ID",
    appVersion: "Version", runningState: "Status", imageName: "Container image",
    artifactUrl: "Download URL", cpuArchitecture: "Architecture", cpuCount: "CPU cores",
    sourceAssetId: "Source device ID", stableId: "Stable ID", id: "ID", os: "Operating system",
    osVersion: "OS version", primaryMac: "MAC address", ipAddresses: "IP addresses",
    ipAddress: "IP address", hostname: "Device name", boardModel: "Model", featureset: "Features",
    memTotalBytes: "Total memory", memoryBytes: "Memory", diskUsedBytes: "Storage used",
    diskTotalBytes: "Storage capacity", usedBytes: "Used", totalBytes: "Capacity", sizeBytes: "Size",
    receivedBytes: "Downloaded", uptimeSeconds: "Uptime", unixNanos: "Device time",
    startedUnixNanos: "Started", stoppedUnixNanos: "Stopped", deployedUnixNanos: "Installed",
    createdUnixNanos: "Created", durationNanos: "Duration", mountpoint: "Mount point",
    filesystem: "File system", cpuMode: "CPU mode", hasGpu: "GPU available",
    hasNpu: "NPU available", gpuVendor: "GPU manufacturer", npuVendor: "NPU manufacturer",
    bluetoothAddress: "Bluetooth address", macAddress: "MAC address", signalStrength: "Signal strength",
    globalCountry: "Country", countryCode: "Country code", framerate: "Frames per second",
    sampleRate: "Sample rate (Hz)", updateRateHz: "Updates per second", timeoutMs: "Timeout (ms)",
    volumePercent: "Volume (%)", nodeId: "Node ID", isDefault: "Default device",
    errorMessage: "Message", success: "Completed", nativePlatforms: "Supported build platforms",
    builderEnabled: "Builds enabled", buildkitAvailable: "Build engine available",
    includeUnpaired: "Include unpaired devices", includeServices: "Include services",
  };
  return special[name] ?? name.replace(/([a-z\d])([A-Z])/g, "$1 $2").replace(/_/g, " ").toLowerCase().replace(/^./, (v) => v.toUpperCase());
}
export function failure(value: unknown): string | undefined {
  if (!value || typeof value !== "object") return;
  const result = value as Record<string, unknown>;
  if (result.success === false || result.errorMessage || result.error) return String(result.errorMessage || result.error || "The device could not complete this operation.");
}
