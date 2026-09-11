# Build-host delivery by chunks

**Ticket:** WDY-2605 · **Builds on:** [2026-08-08 remote build host](2026-08-08-remote-build-host-design.md) · **Related:** PR #1771 (connection retry + progress for the registry push)

## Problem

The build-host feature has four steps. Three of them already had the robustness
the rest of the CLI has; the delivery step did not:

| step | path | chunk-diff + resumable |
| -- | -- | -- |
| upload build context | laptop → build host | yes (chunk store) |
| build image | on build host | n/a |
| **deliver image** | **build host → device** | **no — plain registry push** |
| create container | CLI → device | n/a |

Delivery was `buildctl … --output type=image,push=true` through a loopback
proxy onto the device's registry over the mesh. Every layer went whole, and one
dropped connection lost the whole transfer: on a US build host delivering to a
device in Canada, four consecutive deploys built fine (12/12 stages) and died at
"exporting + pushing layers" with `error reading from server: EOF` at 56–162s,
each retry starting from zero. PR #1771 retries the *connection* and reports how
far the push got, but a half-sent layer cannot be replayed through a proxy — the
body is not seekable and buffering the image is not viable — so a mid-transfer
drop still restarted everything. This is the fix that PR deferred.

Meanwhile the ordinary `wendy run` deploy (laptop → device) has chunk-diffed for
months: `pushLayersByChunks` asks the device what it holds and sends only the
missing chunk bytes into its content-addressed store. Only the build-host leg
skipped it.

## Design

The build host does what the CLI does, from where it stands.

**Build once, export.** `BuildImage` runs one buildctl pass with
`--output type=oci,dest=<contextDir>.image.tar`. The tar is indexed without
reading layer bytes (`readExportedImage`): one scan records each blob's byte
range, then the platform's manifest and config are read back by range. Layers
are addressed by the config's `rootfs.diff_ids`; an export whose config does not
name one per layer is refused rather than mis-paired.

**Deliver per device, by chunks** (`deliverByChunks`). For each target:

1. Dial the target's agent over the mesh — `PeerDialer.DialDevice(asset, mTLS
   port)`, the same byte stream the registry hop used, with mTLS on top pinned
   to the asset id (`PushTLS`). gRPC rides that connection.
2. `QueryChunks{}` as a capability probe. `Unimplemented` → the agent has no
   chunk store → fall back (below) before a layer is touched.
3. `QueryLayers` by diff ID → layers the device already holds are never
   decompressed, chunked, or sent; their headers carry no chunk hashes so the
   device reuses its blob.
4. Decompress + CDC-chunk the remaining layers (`chunk.ChunkStream`, the same
   algorithm as the CLI and the device) into scratch files under the build state
   dir — not `/tmp`, which is tmpfs on WendyOS. Up to four layers at once, and
   once per build: the next device reuses them.
5. Start `PrepareImage(image_name, headers, config)` concurrently. On the device
   it waits for each layer's chunks, assembles and unpacks from the base up while
   later layers still arrive, and registers the image under `image_name`.
6. Per layer: `QueryChunks` → `WriteChunks` the missing ones, gzip-compressed on
   the wire, 64 chunks per stream so at most 4 MiB is in flight unconfirmed.
7. Wait for `PrepareImage`. Strict, as for Compose: the CLI creates the container
   by *name*, so an image that was never registered is a failed delivery.

**Image name.** `localhost:<registry_port>/<repository>` — exactly what the
target's own registry would have stored a pushed image under, and therefore what
the CLI's `CreateContainer` already asks for (`localRegistryReference`). The
registry is backed by containerd's image store and `AssembleImage` normalises
names the same way, so the image lands where a push would have. The CLI is
unchanged and cannot tell how the image arrived.

**Resume.** `WriteChunks` stages each chunk on the device as it lands, so
resume needs no bookkeeping: a transport failure (dial failure, `Unavailable`,
EOF-shaped stream death) re-dials and re-runs the attempt, whose `QueryChunks`
reports only what is still missing. Decompressed layers are kept across
attempts. Four attempts, 1s/2s/4s backoff. Cancellation, an unsupported agent,
and every other status (the device saying something specific — too large,
unsigned, malformed) are *not* retried.

**Fallback.** `errChunkDeliveryUnsupported` — from the probe, or from
`PrepareImage` answering `Unimplemented` (added 2026-08-12; an agent that stages
chunks but cannot register by name cannot finish a create-by-name deploy) — takes
the registry push the feature shipped with: `buildAndDeliver`, a second buildctl
pass that BuildKit's cache turns into a re-export. It is taken for that error
*only*. A genuine failure is reported as one; retrying it over the slower path
would blame the wrong leg, and on a link that just dropped a whole-image push is
the transfer least likely to survive.

**`--chunking`.** The CLI's flag reaches the build host as `BuildSpec.chunking`
(`ChunkingMode`), so it means the same thing whichever machine delivers. `auto`
— and unset, from a CLI that predates the field — is the behaviour above.
`force` turns the fallback into a delivery failure: an agent that cannot take
chunks is the kind of problem the mode exists to surface. `off` takes the
registry route whole — `deliverAllByRegistry`, one pushing buildctl pass per
device and no export — as build hosts delivered before this design. An agent
that predates the field discards it and pushes through the registry regardless,
so the CLI refuses `force` up front against a build host whose
`GetBuildCapabilities` does not report `chunk_delivery`.

**Fleet.** One build, one export, N deliveries. Previously each device cost a
buildctl pass because the push *was* the export. Decompression and chunking are
also once per layer per build: the cache of resolved layers is owned by
`BuildImage` and shared by every device's delivery in turn, so the second
camera costs a diff, not another pass over a multi-GiB layer.

**Progress.** Delivery reports through the `BuildImage` stream as a synthetic
BuildKit plain-progress vertex (`#900 pushing layers to device 214 by chunks`,
`#900 sending 128.0MiB / 1.8GiB`, `#900 DONE 41.2s`). The CLI's renderer folds a
"pushing …" vertex into its "exporting + pushing layers" row and sniffs the byte
counters, so the transfer shows live in the same place it always did — the
renderer learns nothing new. Resume and fallback are announced as detail lines.

**Compatibility.** Two optional proto fields: `BuildSpec.chunking` and
`GetBuildCapabilitiesResponse.chunk_delivery`, both described under
`--chunking` above; a CLI or agent on either side of the change keeps working.
`PushTarget` already carries the asset id,
registry port and repository; the agent's mTLS port is `BuildServiceOptions.
TargetAgentPort`, wired from this device's own `WENDY_AGENT_PORT + 1`.
`BuildImageResult.image_digest`, previously always empty, now carries the
manifest digest. The single-target error contract — `Unavailable` with the
`pushing the built image to the target device failed:` prefix that
`classifyRemoteBuildError` keys on — is kept.

## Security

Unchanged trust boundary. The hop is the same mesh stream with the same
asset-pinned client TLS as the registry push; the target's mTLS gate is the
org-equality interceptor either way, and the container service RPCs are the
ones every `wendy run` already calls. Image content is verified twice on the
build host (each layer's decompressed digest must equal the config's diff ID
before a byte is sent) and again on the device (`WriteBlob` verifies the
reassembled layer against its diff ID; `StageChunk` verifies each chunk's
hash). Device-side signature verification, when enabled, is honoured exactly as
for the CLI path: a `FailedPrecondition`/`DataLoss` from `PrepareImage` fails
the delivery and is neither retried nor pushed around.

## Disk on the build host

The OCI tar (compressed image size) lives beside the context directory for the
duration of the RPC and is removed after. Each layer a device lacks is
decompressed into a scratch file — at most four at a time — that is kept until
the last device has been delivered, so a resumed attempt, or the next device,
costs round trips rather than CPU; all of them are removed then. The peak
working set is therefore one compressed image plus the uncompressed size of
every layer some device lacks,
and it lands on the state directory's filesystem, which need not be the BuildKit
root whose free space `GetBuildCapabilities` reports. The registry-push path
kept the same image in BuildKit's store, so the working set grows by roughly
one compressed image plus those layers.

## Testing

`go test ./internal/agent/services`:

- A fake target agent over bufconn (chunk store, present layers, `PrepareImage`
  that waits for chunks) driven through the `dialTarget` seam: only missing
  chunks are sent; present layers are never chunked; the image is registered as
  `localhost:<port>/<repo>` with the config intact and chunk hashes in order.
- Resume: the fake drops the link with `Unavailable` mid-stream; delivery
  succeeds, every chunk reaches the device exactly once, a fresh stream is
  opened, and the CLI is told.
- Fallback routing: `QueryChunks`/`PrepareImage` `Unimplemented` →
  `errChunkDeliveryUnsupported` with nothing sent; a device refusal
  (`FailedPrecondition`) is neither retried nor fallen back; an unreachable
  peer is retried `deliveryAttempts` times then reported with its reason.
- `BuildImage` end to end with the test binary standing in for buildctl and
  writing a synthetic OCI export: one buildctl pass delivers to two devices; an
  old agent gets a second, pushing pass; the OCI tar and scratch layers are
  gone afterwards; the single-target error contract holds.
- `--chunking` on the build host: `force` against an agent without a chunk
  store is a delivery failure with no pushing pass; `off` runs one pushing pass
  per device, touches no chunk store and writes no export; the agent advertises
  `chunk_delivery`. On the CLI, `force` is refused against a build host that
  does not advertise it; `auto` and `off` are not.
- Two devices share one decompression: the resolved-layer cache and the scratch
  files are unchanged by the second delivery.
- A 1 KiB edit in a 2 MiB layer whose previous version the device holds sends
  under a quarter of the layer.
- An upload that fails after `PrepareImage` already succeeded settles without
  waiting for a second result — the ordering that hung the RPC.
- A failed `QueryLayers` pre-check is logged and every layer is chunked.
- The OCI reader: byte ranges, diff IDs, config, manifest digest; refuses a
  config/layer mismatch and an ambiguous or missing platform; skips
  attestation manifests; follows one nested index and bounds recursion.

Not reproduced on hardware in this PR: doing so needs a multi-GiB image and a
link that drops mid-transfer. The hardware check is the WDY-2605 reproduction —
Spark 3 (US) → ccr1 (Canada) — with a deploy killed at "exporting + pushing
layers" and re-run, expecting the re-run to report the device already holding
most chunks.
