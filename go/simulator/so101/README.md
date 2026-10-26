# Built-in LeRobot SO-101 follower simulator

Create a stopped VM, then provision and start the virtual arm:

```sh
wendy vm create arm-sim --profile so101
wendy vm robot start arm-sim
wendy vm robot status arm-sim
wendy vm robot open arm-sim
wendy vm robot reset arm-sim
```

`wendy discover` → Simulator → Create offers the same profile. Connecting to
`vm:arm-sim` provisions the managed `sh.wendy.simulator.so101` runtime through
the existing VM lifecycle. `wendy vm robot restart arm-sim` recovers a failed
runtime; `wendy vm robot update arm-sim` installs this CLI's source revision.
`wendy vm stop arm-sim` stops this VM. Existing VMs can receive the profile with
`wendy vm robot configure <name> --profile so101`; this does not boot the VM or
replace another robot profile.

The VM agent must advertise `so101-virtual-robot`. If its release predates this
profile, follow the CLI's update instruction. A development binary can be
installed into **this VM only**, even before its runtime can start:

```sh
wendy --device vm:arm-sim device update --binary /absolute/path/to/linux-arm64/wendy-agent
```

The standalone viewer and Wendy Desktop provide six bounded joint sliders,
pause/resume, reset and an eight-second wave.
No walking controls, physical cameras, serial or USB motor backend, real/sim
switch, or ROS policy interface is provided. The `policyBundle` field in the
shared v1 profile identifies this fixed model/controller bundle, not a learned
policy. DDS is disabled for this profile; status reports that explicitly.

## Host requirements

The CLI uses the existing ARM64 WendyOS QEMU backend on macOS and Linux.
Apple Silicon uses HVF. ARM64 Linux uses KVM when available; other Linux hosts
use TCG to emulate the ARM64 guest and will be slower. Install the QEMU binary
and padded ARM64 firmware as described by `wendy vm start --help` and its
startup diagnostics. No platform-specific paths are baked into this runtime.
The container also builds natively for linux/arm64 and linux/amd64, including
Wendy Desktop's existing container backends. The CLI VM guest remains ARM64.
Allow the existing profile default of 4 CPUs/4096 MiB and disk room for a VM
and its image. The first start downloads hash-verified assets and Python/Mesa
runtime dependencies. No GPU or host device access is required.

## API

Use the verified sandbox URL printed by `status`; its host port is allocated
per VM and is not necessarily 8890. All angles below are degrees.

- `GET /api/status`: profile identity, live health, mode, epoch and six joint
  positions/targets/limits. `/api/health` returns 503 on runtime/render failure.
- `POST /api/pose` with `{"joints_deg":{"shoulder_pan":20,"gripper":30}}`:
  validate the entire command before applying it. Unknown joints, non-finite
  values, extra fields and out-of-range targets fail with 422. Partial joint
  updates preserve other targets; motion is rate limited to 0.8 radians/s.
- `POST /api/pause` with `{"paused":true}` pauses physics, keeping the view live.
- `POST /api/reset` with `{}` restores home, clears the demo, resumes physics
  and increments the epoch. `POST /api/demo` starts the bounded wave.
- `GET /api/scene` and `/api/scene/state`: the Go2/G1 geometry and pose protocol.
  `GET /camera.jpg`: current CPU-rendered observer image, not a physical camera.

Browser control requests must have the same origin. The runtime has no CORS
control bridge.

## Verification

```sh
python3.12 -m venv .venv
.venv/bin/pip install -r requirements.txt pytest==8.4.2 httpx==0.28.1
.venv/bin/python tools/fetch_assets.py
PYTHONPATH=. .venv/bin/pytest -q tests
# On Linux, install libosmesa6 and set MUJOCO_GL=osmesa SO101_RENDER_TEST=1.
```

`.github/workflows/so101-simulator.yml` runs Linux host profile/lifecycle/VM
backend and MCP simulator tool tests, plus native amd64/arm64 container physics, API and
OSMesa rendering tests. These contract tests do not substitute for live Linux
host VM boot/provisioning validation. See `validation.md` for actual local
results and any outstanding host checks. Asset attribution and the pinned
revision are in [UPSTREAM.md](UPSTREAM.md).
