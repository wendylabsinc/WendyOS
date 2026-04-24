# Plan: fix Linux container path on WendyAgentMac

## Worktree

- Worktree: `/Volumes/WendyLabs/wendy-agent/kb.fix-linux-container-path`
- Branch: `kb.fix-linux-container-path`
- Branched from: `kb/mac-prototype-fixes`
- Starting point commit: `e03e0717` (`Find Docker Desktop CLI and soften progress fallback errors`)

## Problem summary

Running a Linux Swift example against the mac prototype agent now gets past the
original `docker`-not-found issue, but still fails during container creation.
The CLI falls back from `CreateContainerWithProgress` to legacy
`CreateContainer`, then the mac agent starts the Linux-container Docker path,
but the create call fails while pulling the image.

Current user-visible CLI output:

```text
Pulling image on device... (failed)
█████████████████████████████████ 99.00%
Info: progress reporting is currently not available on this agent; continuing without progress
Error: creating container: Service method threw an unknown error.
```

Agent log output shows the failure happens after `CreateContainer` enters the
Linux Docker backend and starts the image pull:

```text
2026-04-24T09:00:49+0200 info sh.wendy.agent.container: app_name=engineer.edge.examples.helloworld image_name=localhost:5555/helloworld:latest [WendyAgentCore] CreateContainer called
2026-04-24T09:00:49+0200 info sh.wendy.agent.docker-backend: image=localhost:5555/helloworld:latest [WendyAgentCore] Pulling image
```

No more detailed error is surfaced yet.

## Relevant history already landed

1. `f7dd1bf7` — CLI falls back from `CreateContainerWithProgress` to legacy
   `CreateContainer` on older agents.
2. `e03e0717` — mac agent now resolves `docker` from common locations and
   reports searched paths when it cannot find Docker; CLI fallback message was
   softened to an info message.

Those changes fixed the earlier false-negative Docker detection issue, but did
not fix the actual Linux container pull/start path.

## Reproduction context

User repro command:

```bash
cd Examples/HelloWorld
(cd ../../go && make build) && ../../go/bin/wendy run
```

Relevant app config:

- `Examples/HelloWorld/wendy.json`
- `platform: "linux"`

This means the mac agent takes the Linux container path, not the native darwin
file-sync path.

## What is already known

### CLI side

- Swift container builds are pushed to a local registry address returned by:
  - `go/internal/cli/commands/run.go`
  - `resolveRegistryForSwift(...)` in `go/internal/cli/commands/docker.go`
- For the Swift local-build path, the CLI prints values like:
  - `127.0.0.1:51599/helloworld:latest`
- After the build/push completes, the CLI asks the agent to create from:
  - `localhost:5555/helloworld:latest`
- The intended model is: the device/agent should pull from its own local
  registry on `localhost:<registryPort>`.

### Agent side

- `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`
  calls `dockerBackend.pullImage(imageName)` for Linux targets.
- `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerContainerBackend.swift`
  currently does:

  ```swift
  try await docker.pull(image: imageName)
  ```

- `docker pull` errors currently escape as generic Swift errors and appear to
  be turned into gRPC `Unknown`, which the CLI renders as:
  - `Service method threw an unknown error.`

### Host checks already performed

- The host-side registry is reachable via plain HTTP:

  ```bash
  curl http://localhost:5555/v2/_catalog
  curl http://localhost:5555/v2/helloworld/tags/list
  ```

  and it returned:

  ```json
  {"repositories":["helloworld"]}
  {"name":"helloworld","tags":["latest"]}
  ```

- A shell check showed `docker` is not on `PATH` as `env docker`, which led to
  the previous fix.
- A direct call to Docker from this harness failed with:

  ```text
  failed to connect to the docker API at unix:///var/run/docker.sock
  ```

  That failure is from this harness environment and is not enough by itself to
  diagnose the app-side agent behavior, because the GUI app may run with a
  different Docker context/socket setup.

## Primary hypothesis

The mac agent is attempting:

```bash
docker pull localhost:5555/helloworld:latest
```

and that pull is failing for one of these reasons:

1. `localhost:5555` is the wrong registry host from Docker Desktop's point of
   view for this path.
2. Docker is trying HTTPS and rejecting the local plaintext registry.
3. The registry is reachable from the host but not from the Docker daemon path
   used by `docker pull`.
4. The error is ordinary and actionable, but we are losing it because the agent
   does not wrap and surface the underlying Docker failure.

## Findings after code inspection and implementation

### Error surfacing gap confirmed

The Swift mac agent was indeed allowing Docker/backend errors to escape as raw
Swift errors, which GRPC Swift then surfaced to the CLI as generic
`Unknown`/`Service method threw an unknown error.`

That gap existed in two places:

- `ContainerService.createContainer(...)` during Linux-image pull
- `ContainerService.startContainer(...)` / `DockerContainerBackend.createAndStart(...)`

### Most likely root cause: loopback registry + plain-HTTP handling on Docker Desktop

After tracing the end-to-end flow, the most likely actual failure is not the
CLI fallback anymore but the mac agent's `docker pull localhost:5555/...`
path itself.

Why this now looks like the primary issue:

- The Swift CLI build path pushes to the mac agent's own local registry on port
  `5555`.
- The mac agent then shells out to plain `docker pull localhost:5555/...`.
- Docker Desktop commonly treats `localhost:PORT` as a normal registry name,
  which may go through HTTPS expectations.
- Docker's default insecure loopback allowance is reliable for
  `127.0.0.0/8`, not for a `localhost` hostname path.

That makes the most plausible fix on the mac agent:

- rewrite loopback registry references from `localhost:5555/...` or
  `[::1]:5555/...` to `127.0.0.1:5555/...` before `docker pull` and
  `docker run`
- keep surfacing the exact Docker error if the pull still fails after rewrite

This is a better fit than `host.docker.internal` for the current architecture,
because the registry container is started by the same Docker daemon the mac
agent is talking to.

### Changes implemented

1. Added explicit `RPCError(.internalError, ...)` wrapping/logging for Linux
   Docker pull/start failures in:
   - `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`
   - `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerContainerBackend.swift`
2. Added loopback-registry host rewriting in the mac Docker backend:
   - `localhost[:port]/...` -> `127.0.0.1[:port]/...`
   - `[::1][:port]/...` -> `127.0.0.1[:port]/...`
3. Applied the same rewrite consistently to both:
   - `docker pull ...`
   - `docker run ...`
4. Added Swift unit tests covering the rewrite behavior.

### Remaining verification needed on the real mac-agent host

This harness still cannot talk to the local Docker daemon (`/var/run/docker.sock`
missing here), so the final end-to-end confirmation still needs to be done in
WendyAgentMac itself with the normal repro command.

### New finding from live repro

The first post-fix repro exposed the next real blocker cleanly:

```text
error getting credentials - err: exec: "docker-credential-desktop": executable file not found in $PATH
```

That means the mac app is successfully finding `docker` itself, but the
subprocess environment still does not provide a `PATH` that lets the Docker CLI
find its companion credential helper binaries inside Docker.app.

Follow-up fix:

- when `DockerCLI` launches subprocesses, augment `PATH` with the resolved
  Docker executable directory (and fallback Docker binary directories) so
  helper binaries like `docker-credential-desktop` are discoverable too

## First fixes to make in this branch

### 1. Surface the real Docker pull failure

Make the agent wrap the Linux container pull/create/start failures in explicit
`RPCError`s instead of letting them escape as generic unknown errors.

Candidate locations:

- `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`
- `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerContainerBackend.swift`

Expected outcome:

- CLI should show the actual Docker failure message, such as:
  - registry host not reachable
  - insecure registry / HTTP vs HTTPS issue
  - daemon connection issue
  - image not found

Also add structured logging on the agent side for the failure.

### 2. Revisit the image reference used by the mac agent Docker path

Investigate whether `localhost:5555/...` is the right image reference for the
mac prototype's Docker-backed Linux container execution.

Files to inspect:

- `go/internal/cli/commands/run.go`
- `go/internal/cli/commands/docker.go`
- `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerContainerBackend.swift`
- `swift/WendyAgentCore/Sources/WendyAgent/WendyAgent.swift`
- any older mac/legacy agent implementation if available elsewhere in the repo

Possible outcomes:

- keep `localhost:5555/...` but configure the mac agent Docker path correctly
- translate to a different host such as `host.docker.internal:5555/...`
- avoid `docker pull` entirely if the image should already exist locally under a
  different name
- add explicit local-registry/insecure-registry handling for the mac prototype

### 3. Improve the progress fallback UX fully

The inline progress UI still shows:

```text
Pulling image on device... (failed)
...
Info: progress reporting is currently not available...
```

That is better than the earlier hard error, but still misleading because the
operation has not actually failed at that point.

Investigate the TUI completion path so `Unimplemented` can bypass the failed
progress rendering entirely when we know we are about to fall back.

Files:

- `go/internal/cli/commands/run.go`
- `go/internal/cli/tui/progress.go`

This is secondary to fixing the real Linux container pull failure.

## Concrete implementation plan

1. Add explicit error wrapping/logging around Linux-container `pullImage` in
   `ContainerService.createContainer(...)`.
2. If needed, add error wrapping in `DockerContainerBackend.pullImage(...)` so
   the failing image name and Docker command are preserved.
3. Reproduce again and capture the exact surfaced Docker error.
4. Based on the real error, fix the image-reference path or registry handling:
   - host mapping
   - insecure/plain-HTTP handling
   - registry bootstrap assumptions
   - Docker Desktop-specific behavior
5. Add regression tests for the newly exposed errors and for any host rewrite or
   pull-path logic introduced.
6. Optionally clean up the remaining fallback progress UI artifact.

## Suggested verification steps

### Agent-side manual checks

Run on the machine hosting WendyAgentMac:

```bash
/Applications/Docker.app/Contents/Resources/bin/docker version
/Applications/Docker.app/Contents/Resources/bin/docker pull localhost:5555/helloworld:latest
/Applications/Docker.app/Contents/Resources/bin/docker pull host.docker.internal:5555/helloworld:latest
```

If one fails and the other succeeds, that will strongly suggest the needed host
rewrite.

Also useful:

```bash
/Applications/Docker.app/Contents/Resources/bin/docker info
/Applications/Docker.app/Contents/Resources/bin/docker context show
```

### End-to-end verification

From `Examples/HelloWorld`:

```bash
(cd ../../go && make build) && ../../go/bin/wendy run
```

Expected success path:

- Swift image builds and pushes
- CLI prints an info-level progress fallback message if the agent still does not
  implement streaming progress
- `CreateContainer` succeeds
- container starts on the mac agent via Docker backend
- output streams normally

## Acceptance criteria

- Linux container create failures on WendyAgentMac no longer surface as generic
  `Service method threw an unknown error.`
- The real Docker/registry failure is visible in both agent logs and CLI output.
- The HelloWorld Linux example can successfully create and start on the mac
  prototype, or the remaining blocker is at least reported clearly and
  specifically.
- If touched, progress fallback no longer shows a misleading failed progress bar
  before the info message.

## Notes

The branch started as diagnosis-first work. After inspection, the best current
fit is that the real bug is on the mac agent Docker path itself:

- error details were being lost as gRPC `Unknown`
- `localhost:5555/...` is likely the wrong loopback form for Docker Desktop's
  plaintext local-registry pull path

The implemented agent-side rewrite keeps the CLI/device protocol unchanged while
making the mac Docker backend use the loopback form Docker Desktop is most
likely to accept for a local insecure registry.
