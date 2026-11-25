# HelloMesh

This example uses the legacy numeric asset-ID mesh naming scheme. For current
Wendy Net applications with mDNS discovery, start with
[NvidiaPAIR](../NvidiaPAIR/README.md) and the
[Wendy Net service guide](../../docs/guides/wendy-net-services.mdx). Current
service addresses are observer-local; do not derive them from asset IDs.

A **fleet** of WendyOS devices that all reach each other over the mesh.

Every device runs this same app. Each node both **serves** an HTTP endpoint and
**dials every other node** in the fleet — so N devices form a full N×N mesh.
(Sources live in [`./client`](./client); the service is named `node` in
[`wendy.json`](./wendy.json).)

## What it demonstrates

The whole point is one entitlement on the `node` service in
[`wendy.json`](./wendy.json):

```json
{
  "type": "network",
  "mode": "mesh",
  "serviceCIDR": "10.99.0.0/16",
  "ports": [{ "host": 8080, "container": 8080 }]
}
```

- `mode: "mesh"` grants the container **egress to the mesh service CIDR** (so it
  can *dial* peers), and
- `ports` publishes **host port 8080** so peers can *reach* this node's server.

On container start, WendyOS:

1. adds a route inside the container's network namespace: `10.99.0.0/16` → the
   bridge gateway,
2. adds an ACCEPT rule scoped to *this container's* IP in the host's
   `WENDY-MESH` firewall chain, and
3. publishes container port 8080 to host port 8080.

`client/app.py` then serves `hello from <name>` on 8080 and polls every peer in
the fleet, logging each result.

## Why `isolation: "isolated"`

Mesh egress needs the container to have **its own network namespace and
bridge** — that only happens on WendyOS's *isolated* networking path. A plain
host-networked app shares the host stack and has no per-container netns for a
mesh route to live in.

## Topology (a fleet, not a pair)

```
        ┌───────────── device-1 ─────────────┐
        │  HelloMesh / node  (serve + dial)   │
        └───────▲───────────────────┬─────────┘
                │                   │
     device-3 ◀─┘   full mesh       └─▶ device-2
        ▲   every node dials every other node   ▲
        └────────────── device-215 ─────────────┘
```

Each edge is a mesh connection: LAN-direct when both devices are on the same
network, or relayed via the cloud broker when needed.

## Run it

Deploy the same app to every device in the fleet. On each device, set
`MESH_PEERS` to the fleet's asset IDs and (optionally) `MESH_SELF` to that
device's own ID so it skips dialing itself:

```bash
cd examples/HelloMesh

# On device 1 (fleet = devices 1, 2, 215):
MESH_SELF=1   MESH_PEERS=1,2,215 wendy run
# On device 2:
MESH_SELF=2   MESH_PEERS=1,2,215 wendy run
# On device 215:
MESH_SELF=215 MESH_PEERS=1,2,215 wendy run
```

`MESH_PEERS` entries can be bare asset IDs (`215`), `id:port` (`215:8080`), or
full mesh hostnames (`device-215.cloud.wendy.dev:8080`). Bare IDs expand to
`device-<id>.cloud.wendy.dev:8080`.

Those `MESH_*` shell variables reach the container because the `node` service
declares an `env` block in [`wendy.json`](./wendy.json) whose values reference
`${MESH_PEERS}` etc.; `wendy run` expands them from your shell at deploy time.
(An entry that expands to empty is dropped, so unset variables fall back to the
app's built-in defaults.)

**Find your devices' asset IDs** (numbers, 1–65534):

```bash
wendy cloud discover --json   # the "id" field per device
# or the cloud dashboard (https://cloud.wendy.dev)
```

Logs on each node look like:

```
[hello-mesh] serving on 0.0.0.0:8080 as 'device-2'
[hello-mesh] fleet peers: device-1.cloud.wendy.dev:8080, device-215.cloud.wendy.dev:8080 (polling every 5s over the mesh)
[hello-mesh] OK 200 from device-1.cloud.wendy.dev:8080: 'hello from device-1'
[hello-mesh] OK 200 from device-215.cloud.wendy.dev:8080: 'hello from device-215'
```

## See the fleet in the dashboard

Each node reports **mesh telemetry** (metrics + one log line per connection) for
every peer it dials. Open a device's **Mesh** tab in the dashboard to watch, live:
per-peer connections, the LAN-direct vs relay split, bytes tx/rx, dial latency,
and a stream of recent connection log lines. A fleet lights the tab up with one
row per peer.

## Control plane notes

- **Organization-level**: Mesh is **enabled by default**. It will be controllable
  org-wide (admins can disable it via the cloud API) once the cloud-side update
  ships.
- **Device-level**: A device-local `mesh-disabled` file in the agent config
  directory (`/etc/wendy-agent/`) disables mesh serving on that device.
- **Cloud relay**: LAN-direct peering works on this branch, but **both** devices
  must run this branch's agent — the serving side's `MeshDial` RPC is new, so
  dialing an older-agent peer fails. Cloud relay fallback requires a cloud-side
  update (not yet shipped).
- **After an agent restart/reboot**, re-run `wendy run` (recreate) to restore
  mesh networking — the reconcile path recreates the container's resolv.conf so
  it starts, but does not yet rewire CNI networking/mesh egress.

## Debugging the plumbing

```bash
# ACCEPT rule for the container's IP → mesh serviceCIDR
iptables -S WENDY-MESH

# REDIRECT rule (NAT table) that intercepts mesh traffic
iptables -t nat -S WENDY-MESH

# Inside the container's network namespace (substitute the container PID):
nsenter -t <container-pid> -n ip route    # 10.99.0.0/16 via <bridge gateway>
```
