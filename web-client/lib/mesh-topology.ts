// ProtoJSON omits default values and encodes int64/uint64 values as strings.
export type MeshDevice = {
  id: string;
  name?: string;
  deviceType?: string;
  osType?: string;
  platform?: string;
  reporterKey?: string;
  lastHeartbeat?: string;
  organizationId?: string;
  pkiDeviceName?: string;
};
export type MeshPeer = {
  linkId?: string;
  principal?: string;
  deviceId?: string;
  meshAddress?: string;
  transport?: string;
  cost?: number;
};
export type MeshRoute = {
  prefix?: string;
  originRouterId?: string;
  sequenceNumber?: number;
  metric?: number;
  linkId?: string;
  nextHopPrincipal?: string;
  nextHopAddress?: string;
  local?: boolean;
  unreachable?: boolean;
  originPrincipal?: string;
  signedRouteExpiresAtUnixMs?: string;
  originAuthorized?: boolean;
};
export type MeshAnnouncement = {
  principal?: string;
  deviceId?: string;
  name?: string;
  issuedAtUnixMs?: string;
  expiresAtUnixMs?: string;
  revision?: string;
  prefix?: string;
  originRouterId?: string;
  sequenceNumber?: number;
  trustState?: string;
};
export type MeshTopology = {
  selfPrincipal?: string;
  selfTenantId?: string;
  selfDeviceId?: string;
  selfMeshAddress?: string;
  capturedAtUnixMs?: string;
  runtimeAvailable?: boolean;
  status?: {
    runtimeAvailable?: boolean;
    state?: string;
    detail?: string;
    authenticatedPeers?: number;
    gatewayPrincipal?: string;
    configured?: {
      participate?: boolean;
      roam?: boolean;
      shareUplink?: boolean;
      nan?: boolean;
      ble?: boolean;
      ethernet?: boolean;
      infrastructureWifi?: boolean;
    };
  };
  peers?: MeshPeer[];
  routes?: MeshRoute[];
  announcements?: MeshAnnouncement[];
  truncated?: boolean;
};
export type MeshResult = {
  platform?: string;
  status: "ok" | "unsupported" | "unavailable" | "unauthorized" | "error";
  topology?: MeshTopology;
  error?: string;
  observedAt?: string;
};
export type MeshObservation = {
  result: MeshResult;
  topology?: MeshTopology;
  receivedAt?: number;
  attemptedAt: number;
};
export type MeshNode = {
  id: string;
  tenant: string;
  principal: string;
  role: "device" | "operator";
  routingId: string;
  deviceId?: string;
  name: string;
  address?: string;
  inventory?: MeshDevice;
  observation?: MeshObservation;
};
export type MeshLink = {
  id: string;
  source: string;
  target: string;
  observations: { source: string; peer: MeshPeer; stale: boolean }[];
};
export type MeshGroup = {
  tenant: string;
  nodes: MeshNode[];
  links: MeshLink[];
};
export const MESH_STALE_MS = 30_000;
const uuid = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
const principalPattern = new RegExp(
  `^spiffe://wendy\\.sh/tenant/(${uuid})/device/(${uuid})$`,
  "i",
);

export function meshIdentity(principal?: string, routingId?: string) {
  const match = principal?.match(principalPattern);
  if (match) {
    const canonical = principal!.toLowerCase();
    return { id: canonical, principal: canonical, tenant: match[1].toLowerCase(),
      role: "device" as const, routingId: match[2].toLowerCase(), deviceId: match[2].toLowerCase() };
  }
  const operator = principal?.match(new RegExp(`^spiffe://wendy\\.sh/tenant/(${uuid})/operator/([^?#]+)$`));
  if (!operator || !routingId || !new RegExp(`^${uuid}$`, "i").test(routingId) || principal!.length > 1024 || /[\x00-\x20\x7f]/.test(principal!)) return null;
  const instance = routingId.toLowerCase();
  if (instance === "00000000-0000-0000-0000-000000000000") return null;
  return { id: `${principal}#${instance}`, principal: principal!, tenant: operator[1],
    role: "operator" as const, routingId: instance, deviceId: undefined };
}

// An operator principal can have multiple installations. A route's actual
// destination/next-hop address disambiguates them; no device ID is invented.
export function meshNodeFor(group: MeshGroup, principal?: string, routingId?: string, address?: string) {
  const identity = meshIdentity(principal, routingId);
  if (identity) return group.nodes.find(node => node.id === identity.id);
  const candidates = group.nodes.filter(node => node.principal === principal &&
    (!address || node.address === address.split("/")[0]));
  return candidates.length === 1 ? candidates[0] : undefined;
}

export function mergeMeshInventory(previous: MeshDevice[], current: MeshDevice[], retainPrevious = false) {
  if (!retainPrevious) return current;
  const incoming = new Set(current.map(device => device.id));
  const reporters = new Set(current.map(device => device.reporterKey).filter(Boolean));
  return [...current, ...previous.filter(device => !incoming.has(device.id) && (!device.reporterKey || !reporters.has(device.reporterKey)))];
}

export function getMeshObservation(observations: Record<string, MeshObservation>, device: MeshDevice) {
  return (device.reporterKey ? observations[device.reporterKey] : undefined) || observations[device.id];
}

function inventoryIdentity(device: MeshDevice) {
  if (device.platform === "wendy-cli") return null;
  return meshIdentity(
    `spiffe://wendy.sh/tenant/${device.organizationId}/device/${device.pkiDeviceName}`,
  );
}

export function isStale(observation: MeshObservation | undefined, now: number) {
  if (!observation?.topology) return false;
  const captured = Number(observation.topology.capturedAtUnixMs || 0);
  // The status runtime can be available while participation is disabled and
  // no node snapshot exists. Its availability does not imply a capture time.
  const hasRuntimeSnapshot =
    observation.topology.runtimeAvailable ||
    observation.topology.status?.state === "mesh-active";
  return (
    observation.result.status !== "ok" ||
    !observation.receivedAt ||
    now - observation.receivedAt > MESH_STALE_MS ||
    // A successful RPC may contain an old cached runtime snapshot. A missing,
    // invalid or far-future device clock cannot establish fresh peer state.
    (!!hasRuntimeSnapshot &&
      (!Number.isFinite(captured) ||
        captured <= 0 ||
        now - captured > MESH_STALE_MS ||
        captured - now > 5000))
  );
}

export function mergeMeshObservation(
  previous: MeshObservation | undefined,
  result: MeshResult,
  now: number,
): MeshObservation {
  // A successful empty/disabled snapshot replaces old links. A failed request
  // retains its old snapshot, which must be presented as stale.
  if (result.status === "ok" && result.topology) {
    const topology = result.topology.runtimeAvailable ? result.topology :
      { ...result.topology, peers: [], routes: [], announcements: [] };
    return {
      result: { ...result, topology },
      topology,
      receivedAt: now,
      attemptedAt: now,
    };
  }
  const failure =
    result.status === "ok"
      ? {
          ...result,
          status: "error" as const,
          error: "The device returned no topology snapshot.",
        }
      : result;
  return {
    result: failure,
    topology: previous?.topology,
    receivedAt: previous?.receivedAt,
    attemptedAt: now,
  };
}

export function nodeState(
  node: MeshNode,
  now: number,
): { label: string; tone: "good" | "warn" | "muted" | "bad" } {
  const observation = node.observation;
  if (isStale(observation, now)) return { label: "Stale", tone: "warn" };
  if (!observation)
    return {
      label: node.inventory ? "Not observed" : "Reported peer",
      tone: "muted",
    };
  switch (observation.result.status) {
    case "unsupported":
      return { label: "Unsupported", tone: "muted" };
    case "unauthorized":
      return { label: "Access denied", tone: "warn" };
    case "unavailable":
      return { label: "Reporter unavailable", tone: "warn" };
    case "error":
      return { label: "Read failed", tone: "bad" };
  }
  const status = observation.topology?.status;
  if (status?.state === "mesh-active" && observation.topology?.runtimeAvailable)
    return { label: "Local peers active", tone: "good" };
  if (status?.state?.includes("fail"))
    return { label: "Setup failed", tone: "bad" };
  if (
    status?.state?.includes("disabled") ||
    status?.configured?.participate === false
  )
    return { label: "Disabled", tone: "muted" };
  return { label: status?.state || "Runtime unavailable", tone: "warn" };
}

export function buildMeshGroups(
  devices: MeshDevice[],
  observations: Record<string, MeshObservation>,
  now: number,
) {
  const groups = new Map<string, MeshGroup>();
  const nodeMaps = new Map<string, Map<string, MeshNode>>();
  const unknown: MeshDevice[] = [];
  const getGroup = (tenant: string) => {
    if (!groups.has(tenant)) {
      groups.set(tenant, { tenant, nodes: [], links: [] });
      nodeMaps.set(tenant, new Map());
    }
    return groups.get(tenant)!;
  };
  const ensureNode = (
    principal: string | undefined,
    tenant: string,
    name?: string,
    address?: string,
    routingId?: string,
  ) => {
    const identity = meshIdentity(principal, routingId);
    if (!identity || identity.tenant !== tenant) return null;
    getGroup(tenant);
    const nodes = nodeMaps.get(tenant)!;
    if (!nodes.has(identity.id))
      nodes.set(identity.id, {
        id: identity.id,
        principal: identity.principal,
        tenant,
        role: identity.role,
        routingId: identity.routingId,
        deviceId: identity.deviceId,
        name: name || (identity.role === "operator" ? "CLI " : "") + identity.routingId.slice(0, 8),
        address,
      });
    const node = nodes.get(identity.id)!;
    if (address) node.address = address;
    return node;
  };
  for (const device of devices) {
    const observation = getMeshObservation(observations, device);
    const topology = observation?.topology;
    const self = meshIdentity(topology?.selfPrincipal, topology?.selfDeviceId);
    const enrolled = inventoryIdentity(device);
    // Conflicting identity claims never join a graph, even if a bridge regresses.
    const identity =
      self &&
      topology?.selfTenantId?.toLowerCase() === self.tenant &&
      topology?.selfDeviceId?.toLowerCase() === self.routingId &&
      (!enrolled || enrolled.principal === self.principal)
        ? self
        : !topology
          ? enrolled
          : null;
    if (!identity) {
      unknown.push(device);
      continue;
    }
    const node = ensureNode(
      identity.principal,
      identity.tenant,
      device.name,
      topology?.selfMeshAddress,
      identity.routingId,
    )!;
    if (
      node.observation?.topology &&
      (!observation?.topology ||
        (node.observation.receivedAt || 0) > (observation.receivedAt || 0))
    )
      continue;
    node.name = device.name || node.name;
    node.inventory = device;
    node.observation = observation;
  }
  // Only accepted self observations may contribute relationships. Inventory
  // membership, IP subnets and Cloud reachability do not imply mesh links.
  for (const [tenant, group] of groups) {
    const nodes = nodeMaps.get(tenant)!;
    const edges = new Map<string, MeshLink>();
    for (const node of [...nodes.values()]) {
      const topology = node.observation?.topology;
      if (!topology?.runtimeAvailable) continue;
      for (const announcement of topology.announcements || []) {
        ensureNode(announcement.principal, tenant, announcement.name, announcement.prefix?.split("/")[0], announcement.deviceId);
      }
      for (const peer of topology.peers || []) {
        const target = ensureNode(
          peer.principal,
          tenant,
          undefined,
          peer.meshAddress,
          peer.deviceId,
        );
        if (!target || target.id === node.id) continue;
        const pair = [node.id, target.id].sort();
        const key = pair.join("|");
        if (!edges.has(key))
          edges.set(key, {
            id: key,
            source: pair[0],
            target: pair[1],
            observations: [],
          });
        edges
          .get(key)!
          .observations.push({
            source: node.id,
            peer,
            stale: isStale(node.observation, now),
          });
      }
    }
    group.nodes = [...nodes.values()].sort(
      (a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id),
    );
    group.links = [...edges.values()];
  }
  return {
    groups: [...groups.values()].sort((a, b) =>
      a.tenant.localeCompare(b.tenant),
    ),
    unknown,
  };
}

export function routeState(route: MeshRoute, now: number) {
  if (route.local) return "Local";
  if (route.unreachable || route.metric === 65535) return "Unreachable";
  const expiry = Number(route.signedRouteExpiresAtUnixMs || 0);
  if (Number.isFinite(expiry) && expiry > 0 && expiry <= now)
    return "Proof expired";
  if (!route.originAuthorized) return "Origin unverified";
  if (!Number.isFinite(expiry) || expiry <= 0) return "Proof expiry unknown";
  return "Authorized at snapshot";
}

export function elapsed(timestamp: number | undefined, now: number) {
  if (!timestamp) return "Never";
  const seconds = Math.max(0, Math.floor((now - timestamp) / 1000));
  if (seconds < 2) return "Just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  return `${Math.floor(seconds / 3600)}h ago`;
}

export async function collectMeshSnapshots(
  devices: MeshDevice[],
  read: (id: string) => Promise<MeshResult>,
  receive: (id: string, result: MeshResult) => void,
  signal: AbortSignal,
) {
  // Aliases of one physical reporter share a worker. Other reporters can
  // progress while a constrained device establishes its management session.
  const byReporter = new Map<string, MeshDevice[]>();
  for (const device of devices) {
    const key = device.reporterKey || device.id;
    if (!byReporter.has(key)) byReporter.set(key, []);
    byReporter.get(key)!.push(device);
  }
  const groups = [...byReporter.values()];
  let cursor = 0;
  const worker = async () => {
    while (!signal.aborted && cursor < groups.length) {
      const group = groups[cursor++];
      for (const device of group) {
        if (signal.aborted) break;
        let result: MeshResult;
        try {
          result = await read(device.id);
        } catch (error) {
          result = {
            status: "error",
            error: error instanceof Error ? error.message : String(error),
          };
        }
        if (!signal.aborted) receive(device.id, result);
      }
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(3, groups.length) }, worker),
  );
}
