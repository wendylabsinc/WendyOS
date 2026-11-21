const secret = /password|secret|privateKey|enrollmentToken|eabHmacKey|accessToken|refreshToken|pcmData|^data$/i;
export function visibleEntries(value: Record<string, unknown>): [string, unknown][] {
  return Object.entries(value).filter(([key, item]) => !secret.test(key) && hasContent(item));
}
export function hasContent(value: unknown): boolean {
  if (value == null || value === "") return false;
  if (Array.isArray(value)) return value.some(hasContent);
  if (typeof value === "object") return visibleEntries(value as Record<string, unknown>).length > 0;
  return true; // False and zero are meaningful values.
}
export function bytes(value: unknown) {
  const size = Number(value);
  if (!Number.isFinite(size) || size < 0) return "Unknown";
  if (size === 0) return "0 B";
  const unit = Math.min(Math.max(0, Math.floor(Math.log10(size) / 3)), 4);
  return `${(size / 1000 ** unit).toLocaleString(undefined, { maximumFractionDigits: unit ? 1 : 0 })} ${["B", "KB", "MB", "GB", "TB"][unit]}`;
}
export function formatted(value: unknown, key = ""): string {
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (/bytes$/i.test(key) && Number.isFinite(Number(value))) return bytes(value);
  if (/unixNanos$/i.test(key)) return new Date(Number(value) / 1e6).toLocaleString();
  if (/durationNanos$/i.test(key)) return `${(Number(value) / 1e9).toLocaleString(undefined, { maximumFractionDigits: 1 })} s`;
  if (key === "uptimeSeconds") {
    const minutes = Math.floor(Number(value) / 60), hours = Math.floor(minutes / 60);
    return hours >= 24 ? `${Math.floor(hours / 24)}d ${hours % 24}h` : hours ? `${hours}h ${minutes % 60}m` : `${minutes}m`;
  }
  return String(value).slice(0, 2048);
}
export function unavailableFeature(error: unknown, feature: "power" | "wifi") {
  const text = String(error);
  return feature === "power" ? /power modes are only supported on NVIDIA Jetson devices/i.test(text) : /no wi-?fi device found/i.test(text);
}
export function errorMessage(error: unknown) {
  return String(error).replace(/^Error:\s*/, "").replace(/^rpc error: code = \w+ desc = /, "");
}
export function interfaceKind(name: string) {
  if (/^(eth|en)/.test(name)) return "Ethernet";
  if (/^(wl|wlan)/.test(name)) return "Wi-Fi";
  if (/^(wg|tun|tap|mesh|wendy)/.test(name)) return "Tunnel";
  return "Network";
}
