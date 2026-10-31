# NVIDIA PAIR over Wendy Net

Run NVIDIA Personal AI Router 0.1.1 as a headless Wendy container. PAIR keeps
its normal `_nvpair-node._tcp.local.` mDNS advertisement and browser. Wendy Net
carries the service records between enrolled devices and projects them onto each
app's private network.

Each node includes Ollama 0.40.13 and `qwen2.5:0.5b`, with the image and model
manifest pinned by digest. PAIR starts the engine, discovers peers, and routes
independent inference requests across paired nodes that advertise the model.
It does not combine GPU memory or split a model across nodes.

## Requirements

- Linux arm64 or amd64 Wendy devices with current Wendy Net service discovery
  support, enrolled in the same organization.
- NVIDIA GPU access for the supplied manifest. The tested hardware is Jetson
  Orin Nano with JetPack 7.2. Other GPU/driver combinations need validation.
- A CLI with `wendy net` and agents with the app mDNS bridge. An older agent
  accepting `mode: "mesh"` is not sufficient evidence of discovery support.
- Internet access on the build machine to fetch PAIR, the Ollama image, and the
  roughly 398 MB model. The running app needs no engine or model downloads.

## Deploy two nodes

From this directory, replace `DEVICE_A` and `DEVICE_B` with your device selectors:

```sh
wendy json validate
wendy net configure --device DEVICE_A --participate=true --ethernet=true
wendy net configure --device DEVICE_B --participate=true --ethernet=true
wendy net status --device DEVICE_A
wendy net topology --device DEVICE_A
wendy run --device DEVICE_A --detach --yes
wendy run --device DEVICE_B --detach --yes
```

If a test device has local mesh credentials but no Cloud deployment registration,
add `--skip-cloud-registration` to `wendy run`. Mesh authentication still applies.

For ordinary Wi-Fi, use `--infrastructure-wifi=true`. Enable the transports that
both devices can use. Configuration changes can interrupt existing routes.

Check both containers and the app itself:

```sh
wendy --json device apps list --device DEVICE_A
wendy device logs --device DEVICE_A --app sh.wendy.examples.nvidia-pair --no-follow --tail 50
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- pairctl health
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- pairctl rpc discovery:get-nodes
```

Repeat on `DEVICE_B`. The discovery response should include the other node's
UUID and a resolved `ipAddress` in `10.99.0.0/16`. Addresses are local to the observer;
use the address returned on the node that will initiate the connection.
`pairctl health` checks the local proxy, engine health, and successful initial
model warmup. Allow several minutes for the first startup. Check peer
connectivity and inference separately with the fleet check below.

## Pair the nodes

On A, invite B using B's discovered `ipAddress` and the example's cluster
manager port, 14321. Replace the sample address with the value from A's discovery result:

```sh
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- pairctl rpc cluster:invite-node \
  '{"address":"10.99.0.2","port":14321}'
```

The result contains an `inviteId` and PIN. Read B's incoming invitation and
accept it with that PIN within five minutes:

```sh
wendy device attach --device DEVICE_B sh.wendy.examples.nvidia-pair -- pairctl events
wendy device attach --device DEVICE_B sh.wendy.examples.nvidia-pair -- pairctl rpc cluster:respond-to-invite \
  '{"inviteId":"REPLACE_WITH_INVITE_ID","accept":true,"pin":"REPLACE_WITH_PIN"}'
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- pairctl rpc cluster:invite-status \
  '{"inviteId":"REPLACE_WITH_INVITE_ID"}'
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- pairctl rpc nodes:get-initial
```

Check for `state: "paired"`; an RPC response alone does not prove successful
pairing. Wendy organization membership transports the service traffic. PAIR
still performs its own pairing and cluster authentication.

## Networking details

`wendy.json` uses isolated networking with `mode: "mesh"` and
`serviceCIDR: "10.99.0.0/16"`. It publishes these TCP ports without renumbering:

| Ports | Purpose |
| --- | --- |
| 11434, 1234 | PAIR's Ollama and LM Studio proxy facades |
| 14318 | Node information and the node record's SRV port |
| 14319, 14320 | Error synchronization and workload propagation |
| 14321 | Pairing and cluster membership |
| 14322, 14323 | Model inventory and remote engine control |

PAIR puts the additional service ports in TXT records. Wendy preserves TXT data,
so changing only a host-side mapping would leave peers dialing the wrong port.
Keep PAIR's service settings and the mappings consistent. Do not publish engine
backend ports or UDP 5353. The agent handles app mDNS on the private bridge;
publishing UDP 5353 is not how discovery crosses Wendy Net.

PAIR uses its own mDNS sockets, so this app needs no `avahi`, `admin`, Bluetooth,
or Wi-Fi Aware entitlement. The GPU entitlement is for Ollama inference.

The image applies `patches/wendy-mesh-addresses.patch` to the pinned upstream
source. When DNS resolves a peer into Wendy's `10.99.0.0/16` service range, the
patch uses only those resolved addresses. Upstream otherwise prefers `ip=` and
`ips=` TXT fields, which still contain the publisher's private container IP and
can collide with a local container. Ordinary LAN address selection is unchanged.
The patch also resolves pairing callbacks and peer connections by node UUID
through local discovery. This avoids using a remote container address or the
mesh ingress proxy as the peer. PIN verification and certificate pinning remain
unchanged. The Docker build runs address-selection and cluster-manager tests.

An ordinary PAIR desktop on the physical LAN is not equivalent to another mesh
node. Wendy's LAN projection grants access to the advertised SRV endpoint, not
every extra port encoded in PAIR's TXT. Use Wendy Net participants for this
example; LAN discovery alone does not establish complete PAIR interoperability.

## Run inference across the fleet

After both nodes are healthy and paired, check the model inventory and send a
request through PAIR's local client endpoint:

```sh
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- curl -fsS http://127.0.0.1:11434/api/tags
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- curl -fsS \
  http://127.0.0.1:11434/api/generate -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5:0.5b","prompt":"Explain mesh networking in one sentence.","stream":false}'
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- \
  python3 /opt/wendy/fleet_check.py --requests 4 --min-nodes 2
```

The fleet check requires automatic routing, at least two nodes advertising
the model, and the model loaded on every eligible worker. It submits concurrent
requests, checks completed text responses, then reads PAIR's workload records
to report `served_by` counts for the UUIDs
that actually executed them. Run it on an idle app; overlapping client traffic
causes the evidence check to fail. Repeat from B to exercise both directions.
If a manual routing pin is set, clear it first:

```sh
wendy device attach --device DEVICE_A sh.wendy.examples.nvidia-pair -- pairctl rpc proxy:node/select '{"id":""}'
```

PAIR chooses eligible workers using their load and reservations. A single
request may stay local; fleet inference means scheduling separate requests
across workers. Add more nodes by deploying this image, pairing them into the
cluster, and verifying their model inventory. Each worker must fit the model
in its own memory. The small bundled model is for demonstrating the path.

PAIR's remote-client restrictions still apply; publishing the peer ports does
not turn its local client API into a public HTTP API. The commands above run
inside the container. A separate client-facing gateway needs its own access
and authentication design.

## Engines, models, and persistent state

The image contains the engine under `/usr/bin` and model data under
`/opt/ollama-models`. PAIR owns the Ollama process; the wrapper warms the model
with a short generation before declaring readiness. The first CUDA warmup can
take several minutes; its kernel cache persists under `/data/.nv` for later
starts. Mesh mode has no Internet default route, and Wendy
persistent volumes are `noexec`, so runtime engine installation into `/data`
would be unsuitable. To change the model, rebuild with matching `OLLAMA_MODEL`
and `MODEL_SHA256` build arguments. The digest is for the manifest saved by the
pinned Ollama version, which includes its provenance layer.

Inspect `pairctl rpc engine:status '{"engine":"ollama"}'` and
`pairctl rpc engine:logs '{"engine":"ollama"}'` when diagnosing startup or GPU
selection. Ollama's backend listens on loopback; it is not a published mesh
service. Its port is available in `engine:status`.

The `nvidia-pair-state` volume at `/data` retains node identity, trust pins, and
settings across redeploys. Each device must have its own state; do not copy a
node's volume to another device. `pair.py`, adapted from the local `nvidia-pair`
template, owns one broker and keeps its stdin open. `pairctl` talks to that
broker through the container-local `/run/nvpair/control.sock` with mode `0600`.
Do not launch `nvpair-tui` alongside it, which would start a competing broker.

## Local smoke check

```sh
docker build -t wendy-nvidia-pair .
docker run -d --name pair-smoke wendy-nvidia-pair
docker exec pair-smoke pairctl health
docker exec pair-smoke pairctl rpc discovery:get-nodes
docker stop pair-smoke
docker rm pair-smoke
```

Allow startup and model warmup before checking health. Without GPU passthrough,
this local Docker check runs on CPU. Cross-device discovery and fleet dispatch
require the Wendy deployment above.

The upstream source is pinned to
[`13b68115fa2c9c1d94f1ead1358f8d5a527cfecf`](https://github.com/NVIDIA/Personal-AI-Router/tree/13b68115fa2c9c1d94f1ead1358f8d5a527cfecf).
The image includes upstream license and third-party notices. See the
[PAIR broker API](https://github.com/NVIDIA/Personal-AI-Router/blob/v0.1.1/services/nvpair-ui-broker/README.md)
and [Wendy Net service guide](../../docs/guides/wendy-net-services.mdx).

## Verification

Tested on Ci Mesh 1 (WendyOS 0.19.5) and Ci Mesh 2 (WendyOS 0.20.1) on
2026-10-31. Both are Jetson Orin Nano devices with JetPack 7.2 and CUDA 13.2:

- Both containers passed `pairctl health` and discovered each other through
  mDNS at observer-local mesh addresses, despite overlapping private IPs.
- PIN pairing completed on both nodes, and each recognized the other as trusted.
- A stop/start preserved node identity and cluster membership. Each node fetched
  the other's `/v1/node-info` through the resolved mesh address and verified its UUID.
- Both Ollama engines used CUDA and offloaded all 25 model layers to the GPU.
  `/api/ps` reported 456,382,217 bytes of model residency on each GPU.
- Four concurrent requests from Ci Mesh 1 completed in 3.77 seconds, with two
  executed on each node. Repeating from Ci Mesh 2 completed in 3.16 seconds,
  again split two/two. All eight responses completed; PAIR's workload records
  supplied the serving UUIDs. These runs used the bundled Qwen model and the
  fleet check's 128-token output cap.
- Redeploying preserved pairing and the CUDA cache. Both nodes warmed their
  model again and passed health before the final fleet checks.
- The image build passed the patched upstream address-selection and
  cluster-manager tests. The local smoke container exited with status 0 on SIGTERM.
