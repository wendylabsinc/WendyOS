export type DisplayIdentity = { model?: string; device_type?: string };

export function displayIdentity(value: unknown): DisplayIdentity | undefined {
  if (!value || typeof value !== "object") return;
  const row = value as Record<string, unknown>;
  const identity: DisplayIdentity = {};
  if (typeof row.model === "string" && row.model) identity.model = row.model;
  if (typeof row.device_type === "string" && row.device_type)
    identity.device_type = row.device_type;
  return Object.keys(identity).length ? identity : undefined;
}

// Discovery owns permissions and current presence. Only descriptive identity
// can fill its missing fields; a changed device type invalidates an old model.
export function withDisplayIdentity<T extends DisplayIdentity>(
  fresh: T,
  cached?: DisplayIdentity,
): T {
  if (!cached) return fresh;
  const changedType =
    fresh.device_type &&
    cached.device_type &&
    fresh.device_type !== cached.device_type;
  return {
    ...fresh,
    model:
      fresh.model && fresh.model !== "generic"
        ? fresh.model
        : !changedType && cached.model
          ? cached.model
          : fresh.model,
    device_type: fresh.device_type || cached.device_type,
  };
}
