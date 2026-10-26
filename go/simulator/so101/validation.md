# Validation — 12 October 2026

Implementation branch: `codex/so101-simulator`, based on WendyOS
`28ee037fbb7a2ed0f9fa37bf77b2ce3d1b3c3116`.

## Real macOS host evidence

An isolated Apple Silicon QEMU/HVF VM named `codex-arm` was created from
WendyOS 0.19.5 using this worktree's CLI, with
`WENDY_CONFIG_DIR=/tmp/wendy-so101-state`. Creation with `--profile so101`
returned a **stopped** VM and a pinned SO-101 profile. It was then booted on
agent port 50471. A Linux/arm64 development agent built from this change was
installed into this VM only; the published image's older agent does not yet
advertise the new capability.

The actual managed lifecycle built and transferred the runtime, reconciled its
sandbox forward on 8890, and reported healthy/ready. Runtime update, CLI status,
CLI reset, runtime restart, and a subsequent idempotent `robot start` succeeded.
After restart and reconnect, the runtime remained at epoch 1, demonstrating that
reconnecting to a healthy runtime did not reset its world.

`tools/smoke_http.py` exercised the real VM endpoint. Its result was:

```json
{
  "vm_name": "codex-arm",
  "source_digest": "sha256:8bc19149ace5003ba9e9c7069c90c7239c69700936f3f3d2d5c44763ad82b642",
  "healthy": true,
  "ready": true,
  "epoch_before": 1,
  "epoch_after": 3,
  "joint_count": 6,
  "scene_mesh_count": 13,
  "moved_shoulder_deg": 14.282617789661057,
  "rendered_jpeg_bytes": 21108
}
```

This checks virtual joint motion, rejection of an out-of-range target, paused
physics, reset epochs, matching scene/state identity, and an actual rendered
JPEG. The browser displayed the six-joint model, and slider, pause, wave and
reset controls were exercised.

The first live VM run exposed a rendering lock that delayed status and commands.
The final implementation uses separate physics and rendering threads with a
private render data buffer. A deliberate slow-render regression test verifies
that physics and status continue while rendering is blocked. The final VM
returned healthy status and accepted the full smoke sequence. This was a
functional smoke test on a busy shared Mac, not a performance benchmark.

## Automated checks executed locally

- Five Python tests passed inside the shipped Linux/arm64 image with real CPU
  OSMesa rendering. Coverage includes pinned assets, native scene export, atomic
  bounded joint commands, reset/pause, origin/size guards, worker failure, and
  physics responsiveness while rendering is blocked.
- VM storage/backend tests, SO-101 source materialization/tamper detection,
  G1 and ROSMASTER R2 bundle tests passed.
- CLI simulator/profile/lifecycle and gateway scene tests passed, including all
  five profiles in MCP local creation and the SO-101 agent-update, runtime ID,
  restart and identity tables.
- Agent service tests and simulation app-configuration tests passed.
- MCP App TypeScript check, 30 frontend tests, and production bundle build passed.
  The checked-in `desktop_app.html` was regenerated.
- 38 Electron bridge/policy tests and `npm run desktop:check` passed. The Electron
  application itself was not launched or packaged; its source packaging path,
  native SO-101 CPU selection and arm-specific request allowlist were updated.
- Native macOS CLI and cross-compiled Linux/arm64 agent builds passed.

A broad Go rerun under concurrent builds hit synthetic camera decode timeouts and a tunnel
resume assertion outside the simulator changes. Targeted simulator/gateway checks
passed. The camera decode tests passed on a separate rerun after test cleanup.
`TestPushLayersResumingTunnelDropsResumesWithSeveralStreamsOpen` still failed
on rerun because one already-staged chunk was re-sent; the transfer code and
test are unchanged in this branch. It is not included in the passing focused
simulator checks.
The unchanged Go2 `TestMaterializeIdentity` still fails on macOS when renaming a
read-only source-cache directory; its `bundle.go` is byte-identical to the base
revision. SO-101 follows G1's existing macOS-safe publication sequence. The
repository-wide TypeScript configuration also includes unrelated MCP App typing
errors; the independent MCP App check and focused desktop check both passed.

## Linux coverage and remaining gaps

`.github/workflows/so101-simulator.yml` adds native Linux **amd64 and arm64**
host profile/VM-backend/MCP simulator tool/source-cache tests, plus container
builds and real OSMesa/physics/API tests on each architecture. A frontend job
checks the Electron bridge and desktop types.

No live Linux developer host was available for QEMU VM boot/provisioning or
viewer validation. Running a Linux container on this Mac does not establish
Linux-host VM compatibility. The implementation uses the existing ARM64 guest
backend (KVM where available on ARM64 Linux; TCG otherwise), but live Linux
host validation remains outstanding. Linux/amd64 runtime execution is also
pending that CI run. The pinned Python base image index was inspected and
contains both linux/amd64 and linux/arm64 manifests.

## PR follow-up validation

The first GitHub Linux jobs failed compiling the CLI's USB dependency with
`CGO_ENABLED=0`; the workflow now installs the existing CLI's ALSA/libusb
development dependencies and uses native CGO. Viewer CI passed. The separate
vet failure was a Go module proxy HTTP/2 download error, not a vet diagnostic.

Large vendored Three.js modules prevented complete automated API/security review.
They now download at container-build time from a pinned upstream commit with
SHA-256 verification, retaining local browser serving and the shipped MIT
license. Their bytes match the previously vendored files (with the same local
OrbitControls import adaptation). The rebuilt Linux/arm64 image passed all five
real runtime/OSMesa tests, including downloaded viewer hash and HTTP checks.
Source-cache, VM and app-configuration checks passed locally. New GitHub Linux
runs and physical Linux-host VM acceptance remain to be checked.

## Merge with main — 21 October 2026

main removed the ChatGPT gateway and the MCP App, so the gateway scene and MCP
App checks recorded above no longer exist. The MCP `simulator_create` case moved
to `go/internal/cli/mcp/tools_simulator_test.go`.

On a Linux/amd64 host, the rebuilt image passed all six runtime tests with real
OSMesa rendering, including a new check that `/api/pose` accepts the exact joint
limits `/api/status` reports. The VM, MCP, CLI command, app-configuration,
agent-service and simulator Go tests, the desktop type check and 38 Electron
tests passed, apart from one cloud-device resolution test that this branch does
not touch and that also fails on that host on other branches. Live Linux-host
VM acceptance remains outstanding.
