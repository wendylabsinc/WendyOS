# Model watch (P-WDY-258): session handoff

**Date:** 2026-09-25
**Branch:** `ed/model-watch-design` in `/media/work/WendyAgent`, based on `main` at `f36f361af`; not pushed
**Owner:** Ethan, lead of the Linear project P-WDY-258 "Wendy Chat: Let LLM spawn 'any model'"

Read this first when you pick up the work. It tells you where things stand and what not to redo.

## Status

- **Design approved:** `specs/2026-09-25-model-watch-design.md`. Brainstorming split P-WDY-258 into six sub-projects, and this is the design for the first slice, "model watch".
- **Plan approved:** `specs/2026-09-25-model-watch-plan-2-agent-service.md`, the implementation plan for milestone M2. It covers the agent's `WendyModelService` and the `wendy device model` CLI in 17 test-driven tasks.
- **No product code is written yet.**
- **The baseline on this branch is green:** `go build ./go/cmd/wendy-agent` succeeds, and the existing tests the plan extends (`internal/agent/services`, `internal/shared/appconfig`, `internal/cli/grpcclient`) pass.

## What to do next

1. **Read the design and the plan.** In the design, read §1–§5 and §7. In the plan, read the header, Global Constraints and Review Focus.
2. **Execute the plan in order, Task 1 to Task 17**, with `superpowers:subagent-driven-development`. That is the recommended method. Ethan approved the plan but handed off before choosing a method, so confirm with him if unsure. Each task is test-first and ends in its own commit.
3. **Stop at Task 17 Step 6.** That is the Jetson smoke test, and it needs a person: a Jetson with a USB camera, a GHCR token with `write:packages`, and a one-time switch of the fake-host package to public. Give Ethan the steps.
4. **After M2:** open a PR against `main`. Then plan the next milestone with `superpowers:writing-plans`. Each milestone gets its own plan (design §13).

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

These were settled while planning and are already written into the plan:

- **Proto:** times are Unix nanoseconds, as in every v2 agent proto. The plan adds `image_cached` and `last_sequence`, and Task 2 updates the design to match.
- **Camera:** model hosts pin two-plane camera demand through `VideoService`, because the minute-long container sync replaces demand wholesale.
- **Events:** the app data socket gets a record sink so the supervisor sees host records.
- **Reserved app ID:** user apps may not use the prefix `sh.wendy.model.`.
- **Registration:** the service is registered on the mTLS server and the admin socket only, never on the plaintext provisioning port.

## Milestones (design §13)

| Milestone | Scope | State |
|---|---|---|
| M0 | Spikes S1–S5 (detector, Qualcomm engine path, Jetson TensorRT, Pi decode, Mojo interop) | Needs hardware; can run alongside M2; not planned yet |
| M1 | Mojo host and CPU image, run as a plain app on a Pi 5 | Needs S1, S4, S5 |
| **M2** | **Agent service and CLI** | **Planned: next** |
| M3 | MCP tools, notifications, chat event turns | Needs M2 |
| M4 | Jetson and Qualcomm images and adapters | Needs S2, S3 |
| M5 | Hardware validation and write-up | Needs the rest |

## Environment notes for this checkout

- **Committing:** `git config user.name` is unset, so commit with `git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit`. End messages with your own session's attribution lines, not the planning session's `Claude-Session` link shown in the plan.
- **Branch names:** Ethan's branches use the prefix `ed/`.
- **Globs:** the shell the Bash tool runs expands globs, so quote patterns: `grep --include='*.go'`.
- **Toolchain:**
  - Go 1.27.1.
  - `protoc` 36.1, with `protoc-gen-go` v1.36.11 and `protoc-gen-go-grpc` 1.6.2.
  - gcc 16, which `-race` needs.
  - Docker 29.8.1, **without the buildx plugin.** Task 17 needs buildx: `sudo pacman -S docker-buildx`.
- **Generated code:** `make proto` rewrites the header of every generated file. Commit only the new files and `git restore go/proto/gen` for the rest; Task 2 walks through this.
- **GitHub:**
  - `gh` is logged in as EBro912, over ssh.
  - The canonical repository is `wendylabsinc/WendyOS`; `origin` still points at the old name `wendy-agent`, and `gh api` POST calls need the canonical name.
  - The GitHub MCP server failed to connect in the planning session; use `gh` instead.
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

- **Loopback cameras beyond Jetson.** The two-plane camera path needs v4l2loopback. It is proven on Jetson but unconfirmed on Pi 5 and Qualcomm images, which affects M1 and M4.
- **Mojo/MAX port findings are not on this machine.** `docs/mojo-max-port-findings.md` (WendyTemplates PR #100) isn't here. Its findings live in WDY-2906 to WDY-2913 and in `specs/wdy-2906-2913-validation.md`.
- **Limited MAX hardware support.** MAX on the Orin GPU has only been checked for correctness. MAX support for the Qualcomm Hexagon NPU is announced but not shipped.
- **The smoke test needs an interactive shell.** `wendy device shell` may refuse to run a command without a TTY (WDY-2014), so the smoke test uses it interactively.

## Kickoff prompt

Paste this into the new session:

```
Continue P-WDY-258 "model watch" on branch ed/model-watch-design in
/media/work/WendyAgent. Read specs/2026-09-25-model-watch-handoff.md first,
then execute specs/2026-09-25-model-watch-plan-2-agent-service.md task by
task with superpowers:subagent-driven-development. The design and plan are
approved: don't reopen decisions unless a task proves one wrong, and if one
does, stop and ask me. Stop at Task 17 Step 6 (the Jetson smoke test) and
hand it to me.
```
