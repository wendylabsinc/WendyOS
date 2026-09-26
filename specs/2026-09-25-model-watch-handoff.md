# Model watch (P-WDY-258): session handoff

**Date:** 2026-09-25 (updated after milestone M2 was implemented)
**Branch:** `ed/model-watch-design` in `/media/work/WendyAgent`, based on `main` at `f36f361af`; not pushed
**Owner:** Ethan, lead of the Linear project P-WDY-258 "Wendy Chat: Let LLM spawn 'any model'"

Read this first when you pick up the work. It tells you where things stand and what not to redo.

## Status

- **Design approved:** `specs/2026-09-25-model-watch-design.md`. Brainstorming split P-WDY-258 into six sub-projects, and this is the design for the first slice, "model watch".
- **Plan approved and executed:** `specs/2026-09-25-model-watch-plan-2-agent-service.md` (milestone M2: the agent's `WendyModelService` and the `wendy device model` CLI) was executed task by task with `superpowers:subagent-driven-development`. Every task was reviewed, then the whole branch; review fixes are in the branch history.
- **Done:** Tasks 1–17 through Task 17 Step 5 — proto, catalog, file cache, supervisor (leases, restarts, engine builds, events, replay), two-plane camera pins, the data-socket sink, the containerd model-host runtime, the gRPC service, agent wiring, the CLI, and the fake host with its workflow.
- **Not done:** Task 17 Step 6, the device smoke test (needs Ethan; see below). No PR is open.
- **Verified locally at the branch head:** `go build ./...` and `go vet` clean; `go test -race` passes for the models, services, containerd, data, cmd/wendy-agent, grpcclient and appconfig packages and the fake host, and `go test` for the CLI commands — except the pre-existing, machine-specific failures listed under Environment notes.

## What to do next

1. **Run the Jetson smoke test** — plan Task 17 Step 6, with the notes below.
2. **Open a PR against `main`** with the smoke test's outcome in the description (the plan asks for that). Use the canonical repo `wendylabsinc/WendyOS`.
3. **Plan the next milestone** with `superpowers:writing-plans` (design §13: M3 is MCP and chat; M1 is the Mojo host). Carry the follow-ups below into those plans.

### Smoke test notes (Task 17 Step 6)

The plan's Step 6 still applies. What changed or was learned since it was written:

- **Workstation prerequisites.** Docker needs the buildx plugin (`sudo pacman -S docker-buildx`), and this user currently gets "permission denied" on `/var/run/docker.sock` (add the user to the `docker` group or use `sudo`). Step 4's local image build was not run in the implementation session for these reasons; Step 6's `docker buildx build … --push` is the first real build of `go/modelhost/fakehost/Dockerfile`.
- **Image visibility.** The agent pulls model host images anonymously, so `ghcr.io/wendylabsinc/wendy-model-host-fake` must be public (Step 6.1 says how).
- **Camera.** Use a USB camera that delivers native H.264 (the class of camera `Examples/WendyDataModelApp` was proven with). The two-plane path binds only access-unit-aligned native V4L2 H.264; other cameras are now refused at `StartModel` with "camera cannot stream to a model: …".
- **Dev catalog.** The model file in Step 6.2 (this repo's `LICENSE` at `f36f361a…`) was checked: sha256 `c71d239d…0ab4`, 11357 bytes, and the raw.githubusercontent.com URL is public.
- **Expected behaviour after the review fixes.**
  - `wendy device model run` may take up to 10 s to return from its start while a new camera node produces its first frame.
  - Ctrl+C in `run` detaches the watch explicitly, so the model stops at once when it was the only watcher; `wendy device model list` is empty straight away. Only a killed client (`kill -9`, Step 6.5) leaves the model for the 60 s grace, so "gone within 90 s" still applies there.
  - Restarting the agent (Step 6.6) now stops model instances before the servers drain: the `run` stream ends with `state: stopped: the agent is shutting down`, and boot cleanup finds nothing left.

## Decisions already made (do not re-ask)

Ethan approved all of these during brainstorming on 2026-09-25.

| Question | Decision |
|---|---|
| First sub-project | "Chat observes": the chat agent starts a perception model on a device camera and reads what it sees |
| Scenario | Watch and alert with a detector, not an on-device VLM |
| Mojo's role | Mojo runs the pipeline: preprocessing, NMS, tracking and event rules. Each device uses its best engine: TensorRT on Jetson, ONNX Runtime on a Pi 5's CPU, QNN on Qualcomm. MAX becomes an engine adapter once it is proven on a target. |
| Alerts | Only into the open chat; a watch belongs to the chat session that started it |
| Targets | Jetson (Orin Nano, AGX Orin, Thor), Raspberry Pi 5, and Qualcomm. The Mac is out. |
| Models | A curated catalog. The first entry is an Apache-2.0 COCO detector that spike S1 picks. Ultralytics YOLO is excluded because of its AGPL license. |
| Architecture | An agent-owned `WendyModelService`, rather than ordinary apps or data campaigns |

These were settled while planning and are written into the plan:

- **Proto:** times are Unix nanoseconds, as in every v2 agent proto. The plan adds `image_cached` and `last_sequence`, and the design's §5 matches.
- **Camera:** model hosts pin two-plane camera demand through `VideoService`, because the minute-long container sync replaces demand wholesale.
- **Events:** the app data socket gets a record sink so the supervisor sees host records.
- **Reserved app ID:** user apps may not use the prefix `sh.wendy.model.`.
- **Registration:** the service is registered on the mTLS server and the admin socket only, never on the plaintext provisioning port.

Settled while executing M2:

- **Event records may carry `inputs` (Ethan, 2026-09-25).** Design §6.1 has hosts send `model.entered`/`model.left` as `event` records whose `inputs` name the triggering frame, but the data socket let only `prediction` records carry inputs, so every detection was rejected. Ethan chose to widen the socket: `event` records may now name input samples (still validated). Design §6.1 stands; `docs/device/entitlements.md` says so; model input/outcome accounting still pairs only `prediction` records. An end-to-end test pushes host-shaped records through the real socket, validator and supervisor.
- **Commit trailers:** commits end with only `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>` — no `Claude-Session` URL — whichever model wrote them.
- **Review fixes beyond the plan's code** (each brings the code in line with the design): `Start` reuses only admitted, live instances and a reuse restarts the 60 s lease; a reported engine-build failure carries the host log tail; the two-plane teardown re-checks demand under the start lock; `ListHosts` never silently drops a host; `run` detaches before closing its stream; the agent stops models before its servers drain and refuses starts during shutdown; `StartModel` waits for a new camera node's first frame and refuses cameras whose stream cannot carry frame identity; the camera is re-acquired before every host start; the reserved prefix is enforced on the app-name fallback path; failed host starts clean up under a deadline.

## Milestones (design §13)

| Milestone | Scope | State |
|---|---|---|
| M0 | Spikes S1–S5 (detector, Qualcomm engine path, Jetson TensorRT, Pi decode, Mojo interop) | Needs hardware; not planned yet |
| M1 | Mojo host and CPU image, run as a plain app on a Pi 5 | Needs S1, S4, S5 |
| **M2** | **Agent service and CLI** | **Implemented; smoke test pending** |
| M3 | MCP tools, notifications, chat event turns | Needs M2 |
| M4 | Jetson and Qualcomm images and adapters | Needs S2, S3 |
| M5 | Hardware validation and write-up | Needs the rest |

## Follow-ups found during M2 (not fixed)

**For M3 (MCP and chat):**
- Clients must send `StopModel{instance_id, watch_id}` before cancelling a watch stream; cancelling first makes the agent treat the client as lost (60 s grace). This applies to `model_stop` and `/watches stop all`.
- Check requested classes against the catalog's labels before `StartModel` (the CLI's `run` does not yet), so a typo doesn't start a model for 60 s.
- A repeat `StartModel` on a camera the agent already refused returns NOT_FOUND "unknown camera" (the catalog hides it) instead of §9's FAILED_PRECONDITION with the reason; fix by letting `Cameras` remember the refusal reason.
- A pending gap is dropped if the watch ends before the next event; flush it in `Watch.end`.
- `model_service.proto` and design §5 still say `StartModel` "returns at once"; it can wait up to 10 s for a new camera node's first frame.
- `Start`'s camera acquire uses the RPC context, not the instance's, so a start racing a stop or shutdown can wait 10 s and report PREPARING for a stopping instance.
- Watches per instance and label length are unbounded; `ListModels` shows the old state while an instance stops; the CLI says "update the agent" when the service is merely disabled.

**For M1 (Mojo host contract):**
- Send the first `model.status` promptly: the 15 s stall window starts before `StartHost` returns.
- Wait for the ack of the final `failed` status before exiting, or the exit may be treated as a crash and restarted (camera loss must not restart).
- Decide how a host treats a "rejected" ack: the agent acks "rejected" both for invalid records and for episode-capture failures (e.g. a full disk).
- Always send `box`; a missing box parses as zeros. Send an overall `latency_p50_ms` in `model.status` (the proto carries one number).
- **Risk:** the two-plane path binds only AU-aligned native V4L2 H.264 capture. GStreamer output (most USB webcams, and Pi 5 CSI via libcamera) arrives as unaligned byte-stream chunks and is refused, so a Pi 5 camera may never be bindable. Confirm before M1 (design §6.1, spike S4).

**For M4 (Jetson and Qualcomm):**
- Define how the host spells `gpu_arch` in engine file names (e.g. pass `WENDY_GPU_ARCH`), or `engineCached` never matches and `needs_engine_build` stays true. It also ignores the TensorRT version, and the build slot is decided once per start.
- `applyNvidiaCDI` injects the CDI "all" device unfiltered: check the Jetson images for major-81 or camera-engine nodes and guard against extra camera rules. Add lockdown tests for the TensorRT and QNN specs.
- The read-write engine cache is shared by every instance of a model file, so a compromised host could plant an engine another instance loads.

**Robustness and security:**
- Model hosts have no memory or pids limits, and `host.log` and the engine cache are unbounded writes to `/var/lib/wendy`.
- The file cache's per-digest lock ignores cancellation and downloads have no stall timeout; its lock map is never pruned and a crash leaves `.download-*` files.
- A model's camera pin starts the two-plane path for every local camera, not just its own.
- Event `source_id` comes only from the host; fall back to or override with the instance's camera.
- `RemoveHost` deletes a `wendy-model-<id>` container without checking its model label; boot cleanup is best-effort (logged, not retried); the reserved-prefix check is case-sensitive (safe today, since every match downstream is too).
- A host exit racing a stop can briefly report RESTARTING; `HostExit.Err` is not shown; stall and build timers are never stopped (harmless); the crash budget never resets over an instance's life.
- Test gaps: pull/download failure paths, the 8 s/30 s backoffs and 15 s stall threshold, build-slot release paths, `--json` for `list`/`stop`, and several catalog validation branches.

## Environment notes for this checkout

- **Committing:** `git config user.name` is unset, so commit with `git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit`. End messages with the single `Co-Authored-By` line above; no `Claude-Session` link.
- **Branch names:** Ethan's branches use the prefix `ed/`.
- **Globs:** the shell the Bash tool runs expands globs, so quote patterns: `grep --include='*.go'`.
- **Toolchain:**
  - Go 1.27.1.
  - `protoc` 36.1, with `protoc-gen-go` v1.36.11 and `protoc-gen-go-grpc` 1.6.2. (The two new generated files carry a `protoc v7.36.1` header; the rest of the tree says v7.35.1.)
  - gcc 16, which `-race` needs.
  - Docker 29.8.1, **without the buildx plugin**, and this user has no access to the Docker socket.
- **Generated code:** `make proto` rewrites the header of every generated file. Commit only new files and `git restore go/proto/gen` for the rest.
- **Known failing tests on this machine (pre-existing, not from this branch):** `TestStartOneNotesTakeoverOfDefaultedHub` in `go/internal/agent/services` reads the real Elgato Facecam at `/dev/video0`, which lacks a 640x480 mode; four GPU-entitlement tests in `go/internal/agent/oci` fail because this machine has `/dev/kfd` (AMD).
- **GitHub:**
  - `gh` is logged in as EBro912, over ssh.
  - The canonical repository is `wendylabsinc/WendyOS`; `origin` still points at the old name `wendy-agent`, and `gh api` POST calls need the canonical name.
  - The GitHub MCP server may fail to connect; use `gh` instead.
- **Linear:** the MCP server needs OAuth in every new session (`/mcp`, then choose linear and authenticate).

## Linear

- **P-WDY-258** has no issues, milestones or documents yet, and the design and plan are not linked from it.
- **Initiatives:** I-129 "Wendy should offer runtime options for AI Models" (target 2026-12-31) and I-119 "Wendy Chat".
- **Related issues:**
  - WDY-3131 and WDY-3139: artifact identity and fetching. The model file cache follows their content-addressed shape.
  - WDY-3128, WDY-3133 and WDY-3130: motion entitlement, supervised motion and shadow mode. These gate sub-project 6, policies that drive motors.
  - WDY-2558: Mojo 1.0 and MAX with Qualcomm.
  - WDY-940: the Mojo vision demo, which could share preprocessing and NMS code with the host.
  - WDY-3052: whether the `npu` entitlement reaches the secure FastRPC node.

## Open risks outside M2

- **Camera path.** Beyond the native-H.264 limit above, the two-plane path needs v4l2loopback. It is proven on Jetson but unconfirmed on Pi 5 and Qualcomm images, which affects M1 and M4.
- **Mojo/MAX port findings are not on this machine.** `docs/mojo-max-port-findings.md` (WendyTemplates PR #100) isn't here. Its findings live in WDY-2906 to WDY-2913 and in `specs/wdy-2906-2913-validation.md`.
- **Limited MAX hardware support.** MAX on the Orin GPU has only been checked for correctness. MAX support for the Qualcomm Hexagon NPU is announced but not shipped.
- **The smoke test needs an interactive shell.** `wendy device shell` may refuse to run a command without a TTY (WDY-2014), so the smoke test uses it interactively.

## Kickoff prompt

After the smoke test and the PR, paste this into a new session:

```
Continue P-WDY-258 "model watch" on branch ed/model-watch-design in
/media/work/WendyAgent. Read specs/2026-09-25-model-watch-handoff.md first.
M2 is implemented; plan the next milestone (M3: MCP tools, notifications and
chat event turns) with superpowers:writing-plans, carrying the M3 follow-ups
from the handoff. The design is approved: don't reopen decisions unless the
plan proves one wrong, and if one does, stop and ask me.
```
