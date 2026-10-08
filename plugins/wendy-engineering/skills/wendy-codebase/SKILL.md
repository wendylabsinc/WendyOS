---
name: wendy-codebase
description: Use when working in Wendy Labs repos, especially wendy-agent, WendyOS, samples, templates, or cloud docs, and the task needs orientation on Wendy CLI, agent, gRPC, runtime, or app deployment paths.
---

# Wendy Codebase Workflow

Use this workflow for Wendy CLI, `wendy-agent`, WendyOS, and sample app tasks.

## Start from the task surface

1. Identify the current repository and branch with `pwd`, `git status --short --branch`, and `git remote -v`.
2. Read the nearest `AGENTS.md` and `CLAUDE.md` if present. Treat local instructions as authoritative for paths, device access, and publish preferences.
3. Start from the exact user-provided evidence: command, error string, log line, screenshot, review URL, workflow run, or device hostname.
4. Use `rg`/`rg --files` before broad directory walking. Avoid generated proto files unless the task is about generated surfaces.

## Wendy CLI and agent map

In `wendy-agent`:

- CLI entry: `go/cmd/wendy/main.go`.
- CLI command registry: `go/internal/cli/commands/root.go`.
- `wendy run` flow: `go/internal/cli/commands/run.go`.
- Device selection, default-device recovery, mTLS fallback, and update prompts: `go/internal/cli/commands/helpers.go`.
- gRPC client factory: `go/internal/cli/grpcclient/client.go`.
- Build/run providers: `go/internal/cli/providers/`.
- App config and `wendy.json` schema logic: `go/internal/shared/appconfig/`.
- Discovery transports: `go/internal/shared/discovery/`.
- Agent entry: `go/cmd/wendy-agent/main.go`.
- Agent service implementations: `go/internal/agent/services/`.
- Containerd adapter: `go/internal/agent/containerd/`.
- OCI spec and entitlements: `go/internal/agent/oci/`.
- Source protos: `Proto/`; generated Go code: `go/proto/gen/`.

The CLI is a Cobra application. Most device commands resolve a target, create a typed gRPC client, then call one of the agent services. The agent starts a plaintext gRPC server on port `50051`, may start an mTLS server on `50052`, serves a local unix socket at `/run/wendy/agent.sock` (gated by `admin` entitlement), registers agent/container/audio/video/provisioning/telemetry services, and uses containerd for app runtime work.

For app deployment, `wendy run` loads and validates `wendy.json`, resolves a target, builds with the appropriate provider, and deploys to the agent. For container targets it creates/updates a container through `WendyContainerService` then starts it; for native Mac targets it syncs files and optionally runs `brew bundle` before starting the native process. Output is streamed back to the CLI.

## Implementation rules

- Verify runtime wiring, not only config/schema presence. If a feature appears in `wendy.json`, trace from config parsing to the actual CLI or agent code path that consumes it.
- Preserve backward compatibility with older agents. Treat `codes.Unimplemented` as a normal compatibility path where possible.
- Keep host-side and device-side behavior separate. Some fixes belong in CLI plumbing; others belong in agent services, WendyOS image state, or the application.
- Do not publish device credentials, tokens, hostnames, or local machine details into reusable docs or plugin files.
- For Go changes, run `gofmt` on touched files and targeted `go test` packages. Broaden tests when touching shared CLI, gRPC, or runtime behavior.

## Useful local commands

```bash
cd <wendyos-checkout>/go
go test ./internal/cli/commands ./internal/agent/services
gofmt -w <changed-go-files>
wendy-dev --help
```

Use `wendy-dev` when testing the CLI from source if the user's local shell defines it.
