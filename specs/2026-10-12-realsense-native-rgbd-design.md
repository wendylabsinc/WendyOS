# RealSense native RGB-D: the agent reads the camera itself

**Date:** 2026-10-12
**Status:** Design — ready for review
**Builds on:** `specs/2026-09-20-calibrated-frame-sensor-source-design.md` (the
calibrated-frame contract; unchanged by this design),
`specs/2026-10-11-realsense-calibration-protocol-notes.md` (the HWM protocol,
validated on hardware — see its closing section), and WDY-3595 (why the helper
cannot ship).
**Supersedes:** `specs/2026-10-10-realsense-single-owner-design.md` (PR #2228)
and §6's process model + §14 of the calibrated-frame spec (PR #2041's helper).
The *problem* those solved dissolves here; see §3.

## 1. Summary

The calibrated-frame contract (one capture → colour + depth-in-metres +
intrinsics + one timestamp, consumers declare requirements and are refused by
name) survives review and survives this design untouched. What changes is
where the pixels come from.

The previous answer was a helper process linking librealsense. It worked, and
it cannot ship: WDY-3595 established there is no delivery route for it — the
driver/sysext path was adversarially refuted (empty signing key, WendyOS-only
apply path, blocks `--binary` side-loads), Ubuntu robots get nothing, and a
container-built binary may not even load on WendyOS. Meanwhile the SDK's only
real monopoly — reading the factory calibration — was broken on 2026-10-12: a
std-lib-only Go probe read every table from the G1's D435i over
`UVCIOC_CTRL_QUERY`, CRC-valid, physically correct (stereo baseline −50.05 mm
against the D435i's 50 mm build; RGB offset 14.9 mm), with two independent
firmware paths agreeing to float32 precision.

So the agent does it natively, in Go, in-process:

- **calibration** — the HWM vendor protocol, ported from Apache-2.0
  librealsense (§4);
- **depth** — Z16 captured by the agent's own V4L2 loop, the
  `streamV4L2Native` generalisation the 2026-09-20 spec §13.3 already blessed
  as "stage 2" (§5);
- **colour** — not captured again at all: consumed from the `deviceHub`
  producer every other consumer already shares (§3);
- **alignment** — depth→colour projection in Go, from the measured
  calibration (§6).

Nothing is distributed besides the agent binary. The WendyOS/Ubuntu
distribution dilemma, the helper search path, the sysext questions — all of it
evaporates. `wendy-realsense-source` is deleted, not shipped.

## 2. What hardware validation established (and the one surprise)

Full numbers in the protocol notes' closing section; the facts this design
stands on:

1. The HWM channel (XU unit 3, selector 1, 1024-byte buffers) works from
   plain Go on stock Ubuntu with no driver work: GVD (fw, serial), GETINTCAL
   tables 25 and 32, RECPARAMSGET, and depth units via advanced mode all
   answer.
2. **Table sizes vary by firmware.** fw 5.15.1.55 returns table 25 as 512
   bytes, not the 576 the SDK's struct suggests — the trailing reserved bytes
   are absent. Everything load-bearing (baseline f32@160, rect params
   16×float4@256) is inside the 512. Parsers must gate on *content* (header
   version, table_size, CRC) and the offsets they actually read, never on a
   struct's sizeof. An unknown version/size is a refusal with the numbers,
   not a guess.
3. Depth units are **writable** (advanced-mode presets change them;
   a D405 ships 0.0001 m where a D435i ships 0.001 m). They are read at every
   capture start, never cached across captures. The immutable tables (25, 32)
   are cached per (serial, fw) after one CRC-valid read.
4. Device nodes renumber across power cycles (the G1's D435i moved
   video6/8/10 → video0–5). Identification is by sysfs identity — VID 0x8086,
   a D4xx PID table, interface number — never by node index.
5. The old open product question from §13.3 of the 2026-09-20 spec — "a
   SENSOR_NATIVE source has no way to learn its own depth scale, so either an
   operator asserts one or no depth ships" — is **closed**: the scale is now
   measured, from the device, by us. No operator-asserted calibration enters
   the platform.

## 3. The lockout dissolves — why #2228 is superseded, not ported

Both prior designs fought one problem: librealsense claims **all** of the
camera's V4L2 nodes when its pipeline starts, so depth-for-one-app locked
colour away from every other app. #2041 documented the lockout; #2228 built
an ownership registry, a v4l2loopback bridge node, and a two-way
blink-and-reconnect handoff to hide it.

Native capture removes the cause instead of managing the symptom:

- **Colour**: the calibrated producer does not open the colour node. It taps
  the same `deviceHub` producer that serves `StreamVideo`, the raw tap and
  episode capture (`video_service.go:487`, `video_raw_tap.go`). One open per
  node — the sharing rule the agent already enforces — now simply includes
  the RGB-D consumer. Viewers and calibrated subscribers coexist with **no
  handoff, no blink, no bridge node**, because there is no second owner to
  hand off from.
- **Depth**: the Z16 node is opened by the agent's own capture loop. No app
  has a legitimate direct use for a raw Z16 node (the refusal shipped with
  #2041's branch already names it as depth); the agent is its only consumer,
  via the calibrated service.
- **Calibration**: UVC XU control requests, which coexist with streaming on
  both nodes (validated live on the G1 while its DEV apps held the camera).

Consequences for #2228's machinery: the ownership registry, bridge pump,
`CAMERA_PRODUCER_RESTARTED` handoffs and the `BGR3` raw-format row (which
existed only to describe the bridge node) have no remaining problem to solve
and are not ported. Anything already merged toward them is removed in the
deletion pass (§9).

One inherited constraint stays true and must stay documented: colour geometry
is negotiated per-hub. A calibrated subscriber joins the colour mode the hub
is running (or starts the hub at its requested mode), exactly as raw-tap
subscribers do today, and a genuine mode conflict gets today's
"already in use with different stream parameters" refusal. That is sharing's
price, unchanged — not a new lockout.

## 4. The `realsense` package — identification and calibration

New package `go/internal/agent/realsense/`, linux-only files, no cgo, no new
dependencies.

**Enumeration.** Walks a *supplied* sysfs root (the `gpudiscovery` pattern,
so fixtures make tests deterministic): finds USB devices with VID 0x8086 and
a PID in the D4xx table, groups their `videoN` nodes by parent device,
classifies depth node (bInterfaceNumber 00) vs colour node, extracts serial.
Multiple cameras are naturally per-serial. Exposes
`Detect(sysfsRoot) []Camera`.

**HWM transport.** `UVCIOC_CTRL_QUERY` against the depth node: GET_LEN must
read exactly 1024 (anything else: not a DS5 HWM unit → not ours), SET_CUR
sends the 0xCDAB-magic command, GET_CUR reads the reply; opcode echo
negative values map to named errors (−25 TABLE_IS_EMPTY, −28 WRONG_TABLE_ID).
Commands used: GVD, GETINTCAL(25), GETINTCAL(32), RECPARAMSGET, and the
UAMG/advanced-mode depth-units read. Each ported function carries the
Apache-2.0 attribution header naming the librealsense version it was ported
from (v2.59.1); the protocol notes are the normative byte-level reference.

**Control budget.** This camera family is the proven cause of the G1's
tegra-xusb wedge: a storm of failed UVC control retries killed the USB
controller (and the robot's WiFi with it). Calibration reads follow the probe
budget discipline (#2177): one bounded attempt sequence per capture start,
exponential backoff on failure, a hard per-device budget, and **never** a
retry loop around individual control transfers. A camera whose calibration
cannot be read within budget is refused (`REQUIREMENT_UNMET` /
`streamreason`), not hammered.

**The `Calibration` type.** Depth intrinsics per supported mode (table 25
rect params + RECPARAMSGET cross-check when fw ≥ 5.11.9.5), colour intrinsics
at calib resolution plus the documented scale/crop derivation for the other
colour modes, both extrinsics directions (depth→colour as stored;
colour→depth = Rᵀ, −Rᵀt), depth scale in metres, provenance `MEASURED`.
Modes whose intrinsics cannot be derived by the documented rule are refused,
not extrapolated.

## 5. Depth capture — Z16 in the shared pipeline

`streamV4L2Native` (`video_service.go:2553`) already owns a complete V4L2
mmap capture loop; it is hardcoded to `v4l2PixFmtH264`. The loop's fourcc,
buffer sizing and broadcast codec become parameters (one extracted function,
both callers), giving a Z16 capture with no new device machinery. Per §13.3
of the 2026-09-20 spec this was always the intended stage 2; the only part of
that stage it deferred — "where does the scale come from" — is answered by §4.

A **depth hub** per camera serial owns the loop: started when the first
subscriber whose requirements need depth arrives, stopped when the last
leaves, same demand lifecycle as `deviceHub`. Depth mode selection: the best
native depth mode for the negotiated colour geometry (848x480 for 16:9
colour, 640x480 for 4:3 — from the camera's advertised Z16 frame sizes),
never forced to the colour resolution; every D4xx tops depth out at 1280x720
and alignment resamples anyway (the exact defect review found in the helper,
designed out here).

Z16 remains deliberately absent from `rawPixelFormats` — raw subscribers are
still refused with "this is a depth node"; depth reaches consumers only
inside a `CalibratedFrame`, where its scale travels with it.

## 6. Pairing and alignment — in Go, once per frame

**Pairing.** Colour frames arrive from the colour hub tap, depth frames from
the depth hub; both carry kernel monotonic capture timestamps. Frames are
paired by nearest timestamp within half the nominal frame interval — which is
what librealsense's own syncer does; D4xx colour and depth are not genlocked
in the default single-camera configuration, so timestamp matching *is* the
SDK-equivalent behaviour, not an approximation of it. The paired
`CalibratedFrame` carries the colour frame's timestamp as the one
`captured_at_ns`. A colour frame with no depth partner inside the window is
published without a depth plane — and therefore ends the stream of anyone who
required `ALIGNED_DEPTH`, per the contract's mid-stream rule. UVC metadata
nodes (hardware timestamps) are a recorded future refinement, not v1.

**Alignment** is the librealsense `align_z_to_other` algorithm ported to Go:
for each depth pixel, deproject at the pixel's corners (rectified depth: no
distortion), transform by depth→colour extrinsics, project through the colour
intrinsics (applying Brown-Conrady when the coefficients are nonzero — the
G1's unit ships zeros, the code must not assume that), fill the projected
rectangle keeping the nearest Z (occlusion). Cost at 848x480 depth is ~12
MFLOP/frame — ~0.4 GFLOP/s at 30 fps, comfortable scalar-Go territory on a
Pi, trivial on a Jetson. It runs **once per hub frame** when at least one
subscriber gets a depth plane — never per subscriber — and is skipped
entirely when every current subscriber declined depth. `SENSOR_NATIVE`
passthrough (unaligned depth at native geometry) remains expressible in the
contract but is not offered in v1: the one consumer shape we have evidence
for (§2 of the 2026-09-20 spec) rejects unaligned frames.

## 7. The service — same contract, rebuilt lifecycle

The proto surface (`WendyCalibratedFrameService`, `CalibratedFrame`,
requirements, refusal semantics, latest-wins fan-out, the four source-side
rules) is inherited from the 2026-09-20 spec §4–5 and PR #2041 verbatim. The
*implementation* is rebuilt, and the review findings that survive the
helper's deletion become named design constraints:

1. **Shutdown:** hub contexts derive from a service context; the service has
   `Shutdown()` with a WaitGroup, registered in `main.go` beside
   `videoSvc.Shutdown()`. `GracefulStop` must not wait on a calibrated
   producer.
2. **No orphaned subscribers:** every producer exit path calls
   `hub.finish(err)` before returning; finishing is marked under the same
   lock as the last-subscriber check.
3. **No joining a dying hub:** `getOrCreateHub` checks `hub.ctx.Err()`,
   waits on `hub.done`, and retries — the `video_service.go:1222` pattern.
4. **Size checks against the negotiated mode:** both the server's
   oversized-frame refusal and the CLI's receive limit are computed from the
   hub's *negotiated* descriptor when joining, from the request when
   starting — never from the listing default.
5. **First subscriber revalidated:** after the first descriptor arrives,
   `refuseUnmetRequirements` runs over every existing subscriber, so "cannot
   provide" is said up front rather than as a first-frame surprise.
6. **Default-geometry joiners:** a joiner naming the exact geometry the hub
   negotiated is admitted when the hub was started on defaults, and refusals
   print the real numbers.
7. **Layout is a contract:** `--stdout` announces a frame layout once; a
   frame that would change it (depth disappears) ends the stream with the
   reason — fixed-size records or termination, never silent reshaping.
8. **Both planes validated:** colour length is checked against its format
   exactly as depth is; a short plane drops the frame, never ships it.

The CLI (`wendy device camera frames`, `--require`, `--stdout`) carries over,
plus the Swift E2E help/flag coverage the bot asked for.

## 8. Failure honesty

- No RealSense attached → the source is absent from `ListCalibratedSources`;
  streaming it is `FailedPrecondition` naming discovery, never an empty
  stream.
- Calibration unreadable (budget exhausted, CRC bad, unknown table version)
  → refusal carrying the parsed header numbers; with `ALIGNED_DEPTH` or
  `MEASURED_INTRINSICS` required, `REQUIREMENT_UNMET` names which table
  failed and why.
- Depth units read as zero → no depth plane (rule 2 of the contract), with
  the refusal saying the units read zero, not a 1 mm default.
- Camera unplugged mid-run → both hubs end their streams with the existing
  producer-exit reasons; the calibration cache for that serial is kept (it is
  factory data), the units are not.
- Unknown D4xx PID → listed with colour only, depth refused as
  "unrecognised RealSense model <pid>: depth disabled" — new models fail
  visibly, not wrongly.

## 9. What gets deleted

- `go/cmd/wendy-realsense-source/` — the helper, including
  `capture_realsense.go` and its cgo/`-tags realsense` build shape.
- `go/internal/agent/framesource/helper.go`, `wire.go`, `realsense.go`'s
  helper client — the spawn/supervise/length-prefixed-pipe plumbing. The
  `FrameSource` seam itself stays: it is the boundary the second vendor
  (§9a) plugs into.
- #2228's bridge: ownership registry, loopback pump, handoff signalling,
  the `BGR3` row in `rawPixelFormats`.
- The WDY-3595 distribution question in its entirety.

## 9a. Orbbec — the second drawer

Decision 2026-10-12: Orbbec is in scope as the second vendor, as its own
package (`go/internal/agent/orbbec/`) behind the same `FrameSource` seam —
the RealSense drawer's twin, never Intel code growing vendor branches.

Orbbec's line splits into two generations with very different costs:

- **UVC generation** (Gemini 330 series and later): standard UVC devices,
  supported by the open-source OrbbecSDK v2 (open since v2.0.0, Oct 2024,
  UVC-compliant devices only). Same shape of work as this design: a vendor
  extension unit for calibration, Y16/Z16 depth over the shared V4L2
  machinery of §5, alignment via §6 unchanged. The port is "read their open
  SDK, write the drawer" — a repeat of the RealSense exercise.
- **OpenNI generation** (Astra, Astra Pro/Pro Plus — what Yahboom kits
  ship): depth does NOT travel over UVC at all; it speaks Orbbec's legacy
  OpenNI protocol on a vendor USB endpoint (their SDK v2 explicitly dropped
  these; the v1/OpenNI stack is the only support). A native Go port means
  reimplementing that USB protocol — an order of magnitude more than the
  UVC generation. Colour on these devices IS plain UVC and already works
  through the generic stack.

Scope is therefore pinned by hardware, not by brand: **which generation the
fleet's rosmasters actually carry decides the Orbbec plan**, and that is one
`lsusb` on a rosmaster when one comes online (VID 2bc5; the PID separates
Astra from Gemini). UVC generation → full drawer, same contract. OpenNI
generation only → the drawer ships enumeration + an honest refusal ("this
Orbbec speaks the legacy OpenNI protocol; depth needs the containerised SDK"
— the existing app-bundles-SDK pattern keeps working), and a native OpenNI
port is costed as its own decision rather than smuggled in here.

Fate of the open PRs (recommendation, not a constraint — the decision was
explicitly released): **supersede both.** #2041's proto, CLI command,
refusal tests and spec are salvaged by cherry-pick into the new series;
its helper and framesource plumbing are not. #2228 closes with a comment
pointing here. Review investment is preserved — §7's constraint list *is*
that review, promoted into the design.

## 10. Licensing

The HWM protocol port and the alignment port are derived from librealsense
(Apache-2.0). Each derived file carries the Apache-2.0 attribution header
naming upstream and version; no new entry in `scripts/licenses.csv` (that
tracks module dependencies; this adds none).

## 11. Testing

- **Protocol (CI, no hardware):** the raw HWM reply payloads captured from
  the G1's D435i (fw 5.15.1.55) land as `testdata/` fixtures — real bytes,
  not synthesized ones. Golden tests: GVD parse, table 25 at 512 bytes,
  table 32, RECPARAMSGET, CRC acceptance/rejection, the −25/−28 error maps,
  and a truncated-table refusal. A 576-byte synthetic variant guards the
  "gate on content, not sizeof" rule.
- **Enumeration (CI):** sysfs fixture trees — one D435i, two cameras, a
  non-RealSense UVC device, a renumbered-nodes case.
- **Alignment (CI):** synthetic golden cases with hand-computable geometry
  (identity extrinsics; pure translation; known occlusion), plus the
  round-trip invariant depth→colour→depth within one pixel.
- **Pairing (CI):** jittered timestamp sequences; the no-partner case ends a
  requiring stream.
- **Service contract (CI):** #2041's contract tests carry over; new tests
  pin each §7 constraint (shutdown drains, finish-on-every-exit,
  dying-hub rejoin, negotiated-size joiner, first-subscriber revalidation).
- **Hardware-gated (manual, the G1):** concurrent `camera view` +
  `camera frames --require aligned-depth`, both start orders, neither
  refused; parsed calibration equal to the probe's recorded output; unplug
  mid-run ends the requiring stream with the reason; and the agent's numbers
  vs `rs-enumerate-devices -c` if the Thor oracle build is ever wanted
  (recorded as optional paranoia — the CRCs and the dual-path agreement
  already gate correctness).

## 12. Plan decomposition

1. **`realsense` package** — enumeration, HWM transport, tables, budget,
   fixtures from the G1. Pure addition, no behaviour change.
2. **Z16 native capture** — parameterise the `streamV4L2Native` loop, depth
   hub with demand lifecycle.
3. **Pairing + alignment** — the Go port with its synthetic goldens.
4. **Service + CLI** — the rebuilt `calibrated_frame_service.go` honouring
   §7, proto + CLI cherry-picked from #2041, Swift E2E files.
5. **Deletion pass** — §9, including the PR supersession.
6. **Hardware validation on the G1** — §11's manual gate, results appended
   to the protocol notes.

Steps 1–3 are independent of #2041's branch and can land on main
immediately; step 4 is where the surfaces meet.
