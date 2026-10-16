# SO-101 Reader

An ordinary Wendy app for observing a LeRobot SO-101 follower over USB serial on
macOS or Linux. It serves JSON, without a dashboard, ROS dependency or motor SDK.
Python 3.10+ and `lsof` are the native prerequisites; all Python code uses the
standard library. Linux container packaging supplies Python and `lsof`.

The app sends only Feetech protocol-0 READ instructions (`0x02`) for model number
(address 3, two bytes) and present state (address 56, eight bytes). It never writes
motor configuration, enables/disables torque, sets goals, homes, calibrates or
captures cameras. Opening serial `O_RDWR` is necessary to transmit READ packets;
it does not authorize register writes. Serial baud and termios configuration
change the laptop's link, not the motor's registers.

## Run on a developer laptop

From this directory, start an explicitly synthetic reader:

```sh
SO101_MODE=mock SO101_HTTP_PORT=8876 ./run.sh
curl -fsS http://127.0.0.1:8876/v1/observations
```

For physical observations, supply the selected port explicitly. These are
example paths, not discovery defaults:

```sh
# macOS; substitute the selected callout path.
SO101_PORT=/dev/cu.YOUR_SELECTED_ADAPTER ./run.sh

# Linux; a stable by-id symlink works for native execution.
SO101_PORT=/dev/serial/by-id/YOUR_SELECTED_ADAPTER ./run.sh
```

Defaults are six standard SO-101 joint names, motor IDs 1–6 and 1,000,000 baud,
8N1, without flow control. The default endpoint is `http://127.0.0.1:8876`.
Use Ctrl+C or SIGTERM to stop; the worker closes the descriptor, restores the
original termios settings and releases its exclusive claim. No torque command
is sent on shutdown.

Only one process should own a bus. The app checks visible existing owners using
`lsof` before opening and after claiming the device, uses an advisory `flock`,
and sets kernel `TIOCEXCL`. It refuses busy ports and retries without forcing an
owner to exit. Missing `lsof` reports `ownership_unknown`. Ordinary subsequent
opens are excluded; privileged processes can bypass kernel exclusivity, and
`lsof` cannot see descriptors outside its process visibility/permissions.
Do not run competing readers or motor-control apps on the selected port.

The API stays available with no configured port (`missing_port`), a missing tty
(`disconnected`), permissions failure (`permission_denied`), contention (`busy`)
or silent motors (`no_response`). Silence may mean an unpowered arm, incorrect
port/baud/IDs, cable problems or incompatible hardware. USB presence alone does
not distinguish these causes, so the app does not assert an `unpowered` diagnosis.
Retry defaults to one second. Opening a serial descriptor is never robot identity
verification.

## Configuration and pairing contract

Set `SO101_CONFIG` to a JSON file shaped like [examples/binding.json](examples/binding.json).
Paths are resolved against the process working directory; use absolute paths for
externally saved bindings and calibration files. Environment overrides JSON.
Unknown JSON keys and invalid settings fail startup. A missing port is valid and
keeps the API running. Config is loaded at startup; restart the app to change a binding.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `SO101_MODE` | `physical` | `physical` or explicitly synthetic `mock` |
| `SO101_PORT` | unset | Explicit native serial path; no automatic selection |
| `SO101_BAUD` | `1000000` | Laptop link speed: 9600, 19200, 38400, 57600, 115200, 500000 or 1000000 |
| `SO101_IDS` | `[1,2,3,4,5,6]` | JSON array of six unique unicast motor IDs, 1–253 |
| `SO101_JOINTS` | standard names | JSON array of six names corresponding to IDs |
| `SO101_BINDING` | `{}` | JSON object of caller-supplied identity/provenance metadata |
| `SO101_CALIBRATION_FILE` | unset | Optional JSON metadata; never applied to ticks |
| `SO101_HOST` | `127.0.0.1` | Numeric IPv4 listen address; set deliberately for remote access |
| `SO101_HTTP_PORT` | `8876` | HTTP port, 1–65535 |
| `SO101_INTERVAL` | `0.5` | Pause in seconds after a successful/partial scan |
| `SO101_TIMEOUT` | `0.1` | Maximum seconds for each READ, 0.02–1 |
| `SO101_STALE_AFTER` | `5` | Per-joint freshness deadline in seconds |
| `SO101_RECONNECT_DELAY` | `1` | Pause after open failure or an entirely silent scan |
| `SO101_MOCK_FAILED_IDS` | `[]` | Mock-only JSON array to simulate missing motors |
| `SO101_PYTHON` | `python3` | Interpreter used by `run.sh` |

Polling is sequential, not simultaneous. With six unresponsive motors and the
default timeout, a scan takes up to about 1.2 seconds plus overhead. Stop checks
occur every 20 ms during exchanges. Owner checks have a five-second subprocess
deadline; best-effort adapter metadata lookups have a two-second deadline.
Cancellation is checked after each owner check, before continuing to open or
configure the port. A retained port is reopened
after fatal I/O failures or a fully silent scan; there is no baud sweep or scan
of other ports.

A future pairing integration can pass `SO101_PORT` plus `SO101_BINDING`, or save
the JSON config and supply `SO101_CONFIG`. The app returns binding metadata
verbatim as operator-supplied information. Native Linux additionally reads USB
vendor/product/serial metadata from sysfs when accessible; native macOS attempts
the same via I/O Registry. Missing metadata stays `source: unavailable`.
`identity_verified` and `robot_identity_proven` remain false: USB adapter identity,
motor model and a configured joint list do not prove arm identity. This example
does not start itself through `device pair` or modify CLI/Agent behavior.

## Local API v1

| Method and path | Behavior |
| --- | --- |
| `GET /healthz` | 200 while the reader worker is alive; 503 on worker failure/stopping. Independent of hardware. |
| `GET /readyz` | 200 only for complete, fresh, fault-free **physical** observations; 503 otherwise, including mock. |
| `GET /v1/observations` | 200 with a versioned snapshot, even when hardware is unavailable; inspect readiness/state/errors. |

Responses use `application/json` and `Cache-Control: no-store`. Unknown paths
return 404; non-GET methods are unsupported. There are no command endpoints.
The endpoint has no authentication or CORS permission; its safe default is IPv4
loopback. Host networking in Linux preserves that default. Publishing beyond
loopback is an explicit operator choice. A Wendy TCP readiness probe establishes
process listening only; it is separate from `/readyz` and hardware readiness.

Every observation has `schema_version: 1`, a UTC `generated_at`, a completed-scan
`sequence`, `source` (`physical_serial` or `mock`), `synthetic`, `read_only`,
`process`, `observation_ready`, `hardware_ready`, `connection`, `scan`,
`calibration` and six `joints`. Additive fields may appear within v1. Consumers
must reject unknown schema versions. A snapshot may contain joints from different
instants while a scan is underway; use **each joint's** timestamp and age.

Each joint contains:

- `id`, `name`, `model_number` (numeric register value) and known model name or null.
- `position.raw_word`: authoritative unsigned 16-bit Present_Position register
  word, with no masking, centering, clamping or host-side homing adjustment.
- `position.ticks`: sign-magnitude bit-15 decoding for a recognized STS/SMS
  model, unit `encoder_tick`. Unknown/unreadable models retain `raw_word` and
  return null ticks. `position.calibrated` is always false.
- `voltage` in `V`, `temperature` in `degC`, and raw `packet_error_bits` in
  `bitmask` from the model/state response status bytes (ORed within this joint's
  scan). This is the packet error field, not a separate read of Status register 65.
- `observed_at`: UTC timestamp of the successful state READ, or null;
  `age_seconds`: monotonic elapsed age, or null; `fresh`: state bytes are within
  the configured freshness deadline. Freshness does not erase errors.
- `errors`: structured `{code,message}` failures. `fresh: true` alone is
  insufficient: require empty errors, decoded ticks and physical readiness.

Failed fields are null rather than copied from an earlier scan. If model READ
fails but state succeeds, fresh raw words/health remain available with an error
and null decoded ticks. A fatal disconnect invalidates all previous readings.
Stale retained readings are explicitly marked `fresh: false`; readiness fails.
Nonzero motor packet error bits fail readiness even when position bytes arrived.
`connection.state` may be `connecting`, `missing_port`, `ready`, `degraded`,
`no_response`, `disconnected`, `busy`, `permission_denied`, `ownership_unknown`,
`invalid_port`, `unsupported_baud`, `serial_error`, `stale`, `worker_failed` or
`stopping`. Individual protocol/checksum/timeout/model/fault errors are in joints.

The present-position register can already reflect homing/mode settings stored in
the motor by another tool. This reader does not inspect or change those settings,
unwrap encoders, infer physical centers or compute calibrated model angles.
The [LeRobot Feetech table](https://github.com/huggingface/lerobot/blob/main/src/lerobot/motors/feetech/tables.py)
defines the register layout, sign bit and recognized model numbers. See also
[LeRobot's Feetech implementation](https://github.com/huggingface/lerobot/blob/main/src/lerobot/motors/feetech/feetech.py)
for its normalization/calibration context. Model-angle mapping remains a later
verified integration step. Optional calibration JSON is returned as
`file_loaded_unverified`, with SHA-256 and `physically_verified: false`,
`applied_to_readings: false`. Invalid/missing files report `load_failed` without
blocking raw readings. Loading metadata is not physical calibration verification.

Consume physical readings:

```sh
python3 examples/consume.py http://127.0.0.1:8876
```

Or use the documented JSON fields directly:

```python
import json, urllib.request
with urllib.request.urlopen("http://127.0.0.1:8876/v1/observations", timeout=3) as r:
    snapshot = json.load(r)
assert snapshot["schema_version"] == 1
if snapshot["hardware_ready"] and not snapshot["synthetic"]:
    for joint in snapshot["joints"]:
        if joint["fresh"] and not joint["errors"]:
            print(joint["name"], joint["position"]["ticks"], "encoder_tick")
```

## Wendy deployment

### Native macOS

The checked-in `wendy.json` syncs this app and launches `run.sh`. No container or
Linux serial entitlement is used on Darwin. The selected Mac must already have
Python 3.10+, `lsof`, and a Mac Agent advertising `native-process`; `run.command`
is capability-gated by Wendy. No automatic interpreter/adapter installer is
included. `SO101_PYTHON` can select an installed interpreter explicitly.

```sh
wendy json validate .
wendy run --device YOUR_MAC_AGENT --yes --detach --no-restart \
  --env SO101_MODE=mock --env SO101_HTTP_PORT=18876
# Physical: replace mock with physical and pass --env SO101_PORT=/dev/cu.SELECTED.
```

For a JSON binding/calibration file, put the file inside a synced `files` entry
and use its app-relative path, or refer to an absolute path already on the target.
No host file is silently copied because an environment variable names it.

### Linux native and Linux Wendy containers

Direct `./run.sh` on a Linux developer laptop is the smallest path. Use the user's
normal tty permissions (often a dialout group), and install `lsof` explicitly if
absent. No root requirement, ROS, container or Agent is needed for direct runs.

For ordinary Wendy container deployment, generate a separate staged project:

```sh
# Test without any serial device entitlement:
python3 scripts/package_linux.py --mock --http-port 18876
wendy json validate .build/linux
wendy run --prefix .build/linux --device YOUR_LINUX_AGENT \
  --build-type docker --yes --detach --no-restart

# Physical: on the Linux host, first confirm no app owns the chosen tty.
lsof /dev/ttyACM0
# Proceed only when the selected tty has no owner. Do not stop another app.
python3 scripts/package_linux.py --port /dev/ttyACM0
wendy json validate .build/linux
wendy run --prefix .build/linux --device YOUR_LINUX_AGENT \
  --build-type docker --yes --detach --no-restart
```

The generator adds exactly one `serial` entitlement matching the explicit tty,
plus host networking and the matching HTTP declaration. It never grants camera,
GPU, raw USB or admin access. Without `--port`, the physical configuration is
valid and reports `missing_port`. A by-id binding must be resolved to its current
ttyACM/ttyUSB node on the Linux host for Wendy's serial entitlement; node changes
may require regenerating/redeploying. Native execution can reopen a stable by-id
symlink directly. Container bind mounts can retain an obsolete device inode
after USB hotplug; redeploy that app when needed.

**Container ownership limitation:** `lsof` sees only the container's process
namespace. Before deploying physical mode, check port ownership on the Linux
host. Kernel `TIOCEXCL` still excludes subsequent ordinary opens, but cannot
identify pre-existing nonexclusive host descriptors hidden by the namespace.
Prefer native laptop execution when host ownership cannot be established.

Inspect normal Wendy app lifecycle and logs with the same explicit target:

```sh
wendy --json device apps list --device YOUR_AGENT
wendy --json device logs --device YOUR_AGENT --app sh.wendy.examples.so101-reader --no-follow --tail 30
wendy device apps stop sh.wendy.examples.so101-reader --device YOUR_AGENT
```

Detached deployment success is not hardware readiness. Fetch `/healthz`,
`/readyz` and `/v1/observations` separately. A remote target's loopback endpoint
requires an appropriate tunnel/local consumer; its reported HTTP URL may not be
reachable from your laptop. QEMU user-network forwarding reaches the guest's
network interface rather than its loopback. To test the mock API through that
forward, deliberately add `--env SO101_HOST=0.0.0.0` to the VM deployment; the
host-side forward remains on host loopback. This expands the guest listen scope
and is never the app default. Ports 8765, 8891 and 50161 are not used by this app.

Use a Linux **Agent** target for these manifests. The installed CLI's local
`--device docker` provider did not forward manifest environment or network
entitlements during validation; that provider is not the documented deployment
path. Direct Docker testing can supply its own `-e` and networking flags.

## Validation

```sh
python3 -m unittest -v
docker build -t so101-reader-test:local .
docker run --pull=never --rm --network none \
  --mount type=bind,source="$PWD",target=/tests,readonly --workdir /tests \
  --entrypoint python3 so101-reader-test:local -m unittest -v
```

Tests use only mock transports and synthetic PTYs. On macOS the IOSSIOSPEED ioctl
is stubbed for PTYs, which do not implement the USB serial driver's custom baud
operation. Linux PTYs exercise actual B1000000 termios settings. No test opens a
physical USB device. See [VALIDATION.md](VALIDATION.md) for evidence and remaining
host/hardware/deployment gaps for this change.

Protocol design was audited against the actively used `demos/wendy-arms`
MotorProbe/RobotStatus/RobotProfile sources without modifying them. The existing
SO101 and SO101TeleopMulti sources supplied Linux entitlement precedents; their
torque/goal writes are intentionally absent here, including the leader's
startup torque-off operation.
