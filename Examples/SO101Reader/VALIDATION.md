# Validation evidence — 2026-10-12

Initial validation scope: only `Examples/SO101Reader`. No changes to CLI, Agent core, simulator,
viewer, `demos/wendy-arms`, or the other SO-101 examples. No physical motor port
was opened during that initial validation, and no torque/configuration/goal writes were performed.

## Passed

- **macOS native:** Python 3.14; 30 unit/integration tests, plus compilation with
  `py_compile`. Direct `./run.sh` process smoke on isolated port 18877 exercised
  explicit mock, physical-with-missing-port, and nonexistent-port cases. Process
  health stayed healthy while hardware readiness remained false; SIGTERM exited
  cleanly each time.
- **Linux arm64 container:** Debian Bookworm / Python 3.12; the same 30 tests
  passed with networking disabled and source mounted read-only. Real Linux PTYs
  exercised B1000000 serial setup, fragmented replies, timeout, disconnect,
  exclusive claim/release and cancellation. macOS PTYs require an IOSSIOSPEED
  stub; actual macOS USB custom-baud behavior remains unverified.
- **Protocol/data:** exact READ whitelist and packet bytes; prohibited addresses,
  broadcast/invalid IDs; malformed headers/checksums/lengths/IDs; sign-magnitude
  boundaries including 0, 2048, 4095, 4096, 32767, negative zero and -32767;
  partial responses, unknown model, model-read failure with raw position retained,
  motor error bits, silent bus, stale data, disconnect invalidation and reconnect.
- **API/config/lifecycle:** versioned response, no-store, unsupported paths/methods,
  process health distinct from hardware readiness, mock cannot become physically
  ready, occupied HTTP port fails before hardware starts, optional calibration,
  invalid config/non-finite JSON, injected permission/busy/open failures, real
  visible pre-existing nonexclusive PTY owner via `lsof`, worker failure, optional
  adapter metadata failure without losing port lifecycle, and SIGTERM shutdown.
- **Manifests:** `wendy json validate` passed for native Darwin, generated Linux
  mock, and generated Linux physical config with an explicit serial entitlement.
- **Docker build:** the ordinary app Dockerfile built successfully for Linux arm64.
- **Wendy Linux Agent deployment:** deployed only this reader's explicitly mock
  app (`sh.wendy.examples.so101-reader`) to the already-running `vm:lerobot-sim`,
  using port 18876. Detached run returned `started` / `not_checked`; app inventory
  and finite logs were checked separately. With explicit `SO101_HOST=0.0.0.0`
  for QEMU ingress, the host-loopback forward returned `/healthz` 200, six fresh
  synthetic joints and advancing scan sequences. `/readyz` correctly returned
  503, `observation_ready: true`, `hardware_ready: false`, `source: mock`.
  The simulator viewer app on port 8891 remained running. This reader's test
  app was stopped and removed after validation; no unrelated app/VM/Agent was stopped.
  Wendy/QEMU retained the host-loopback forward on 18876 after app removal; no
  reader serves it. The VM was left running to preserve the existing viewer.

## Observed limits and outstanding validation

- **Mac Agent deployment unavailable:** no local compatible Mac Agent was
  listening. The discovered remote Mac Agent did not answer the authenticated
  deployment handshake. No agent update, identity unpin, service restart or
  workaround runtime was performed. Native execution passed; `run.command`
  packaging is structurally valid and its `native-process` feature gate was
  verified in current CLI/Agent source, but a successful Darwin Agent deployment
  is still required.
- **Physical USB validation unavailable:** a USB tty was listed initially, then
  disappeared before a probe could be considered. No physical probe was made.
  Actual USB-driver baud behavior, physical readings, unplug/replug recovery,
  power-off diagnosis and hardware fault conditions need a later exclusive,
  read-only session. Permission and busy state handling has test coverage, not a
  real USB-host permissions acceptance test.
- **Linux developer HOST validation unavailable:** Linux runtime testing used
  containers and a WendyOS Linux VM, not a physical Linux laptop with a USB arm.
  Native Linux sysfs metadata, actual laptop tty permissions and USB hotplug need
  host acceptance. No physical Linux host was selected or modified for this task.
- **Container ownership:** `TIOCEXCL` and `flock` work, but process namespace
  isolation hides pre-existing nonexclusive host descriptors from container
  `lsof`. Perform the documented host ownership preflight before physical
  container deployment, or use native laptop execution. A device-node bind mount
  may need redeployment after USB re-enumeration.
- **Local Docker provider:** the installed CLI's `--device docker` path started
  the image but omitted manifest environment and network settings. The resulting
  process correctly reported physical `missing_port` at its default port, rather
  than inventing data. Use the validated Linux Agent deployment path or explicit
  Docker runtime flags. The disposable local test container was removed.
- **Loopback/VM routing:** a loopback-only guest listener is intentionally
  unreachable through QEMU's interface-based forward even when Wendy reports an
  HTTP URL. Keep the native loopback default; use an explicit listen-scope choice
  or a suitable tunnel/local consumer when remote access is needed.

## Integration handoff

Pairing supplies an explicit serial port and optional operator binding through
`SO101_PORT` / `SO101_BINDING`, or a saved JSON file through `SO101_CONFIG`.
The app does not auto-start from device pairing. Config is read at startup.
Linux Wendy packaging additionally needs a matching current ttyACM/ttyUSB
entitlement; stable by-id aliases work directly for native Linux execution.

The viewer can consume `GET /v1/observations` at the configured endpoint. Gate on
schema version, `source`, `synthetic`, `hardware_ready`, per-joint `fresh` and
errors. Preserve raw words/ticks and provenance. A separately verified mapping
from motor coordinates to calibrated SO-101 model angles remains outstanding;
there are deliberately no assumed 2048 centers or derived angle fields here.

## Subsequent physical macOS acceptance

- Read-only native execution on the explicitly selected SO-101 USB callout port
  returned complete, fresh, fault-free readings from all six STS3215 motors over
  advancing scan sequences. The reader stopped and released its serial claim.
  No torque, goal, homing, calibration or configuration writes were sent.
- The original whole-IOService metadata query timed out on this Mac. Scoping
  I/O Registry to USB-host-device subtrees returned the selected adapter's serial,
  vendor and product metadata. The updated reader then reported those fields
  alongside all six physical readings and the supplied calibration file digest.
  A locally selected test binding was supplied explicitly; this did not exercise
  CLI pairing, and adapter metadata is still not robot identity verification.
- The host's `lsof` scan sometimes exceeded the original two-second deadline.
  Numeric-only lookup and a five-second bound avoid unnecessary name resolution
  and accommodate the observed latency. Unknown or busy ownership still refuses
  the port. Cancellation after each owner check prevents continuing to open or
  configure the port after a stop request.
- All **35** local unit/integration tests passed after these fixes, including
  USB-subtree identity inheritance, missing ports, metadata timeout, ownership
  timeout preventing serial open, and cancellation during ownership checking.
- Model-angle mapping, physical unplug/replug recovery, fault injection, Darwin
  Agent deployment and physical Linux laptop acceptance remain outstanding.
