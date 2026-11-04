# Wendy Scout

**Autonomous bug hunting on your Wendy device.**

Deploy one normal Wendy app on an ARM64 Thor. Scout runs a pinned local GPU
coding model and Pi, reads recent public WendyOS commits/releases/issues,
chooses experiments, compares verified nightly and stable CLI releases on the
same device, and retains evidence. Its trusted publisher can file twice-replayed
compatibility findings with `ai-found`, after all-state duplicate checks. It
also supports honest no-bug, already-fixed, unsupported and blocked outcomes.

Scout currently targets **Development-enrolled** devices. It preserves the
installed Agent, OS and power mode. Camera/audio tools enumerate metadata only;
Scout has no capture or physical-control capability in its model-facing tools.
It does not install host tooling or run a GitHub runner, coordinator or cron.

## Deploy and control

From this directory in a WendyOS checkout, with your normal operator CLI already
connected to your Development Thor:

```sh
wendy --json --device <your-dev-thor> run --build-host <your-dev-thor> --detach --yes
wendy --json --device <your-dev-thor> device apps list
wendy --json --device <your-dev-thor> device apps stop sh.wendy.scout
wendy --json --device <your-dev-thor> device apps start sh.wendy.scout
```

The Thor must already have its Wendy builder enabled. Start streams output by
default; `--detach` also changes the restart policy to unless-stopped. The app
has two services managed as **one app group**: `control` and `worker`. Stop the
bare app ID to stop both. A restart resumes durable evidence and progress.
A test-owned scratch container uses a random installation-specific ID, no
restart policy, and a supervisor liveness lease. Graceful shutdown deletes it;
a forced kill makes it exit, and the next start recovers the recorded leftover.
No existing application is selected for a test.

The first start downloads an 18.6 GB GGUF **on the device**. `control/pins.json`
pins the model revision/quantization/digest, official Ollama ARM64 CUDA image and
Pi release. The model server must prove GPU layer offload before sessions begin.
The server binds host loopback only, without a published HTTP entitlement.
Pi has exactly one local provider; no frontier/subscription fallback exists.

Volumes survive redeploys:

| Volume | Purpose | Service |
|---|---|---|
| `wendy-scout` | Verified weights, CLI cache, root-only raw evidence/findings and publisher secret | control |
| `wendy-scout-ipc` | Root-only local control socket | both trusted service supervisors |
| `wendy-scout-work` | Isolated Pi sessions, offline experiments and kernel preflight evidence | worker |

Public release metadata determines channels from `prerelease`, never tag age or
name alone. Scout pins latest stable, previous stable and latest nightly per
session. Release/context refresh happens between sessions on a bounded interval;
transient fetch failures back off. Cached artifacts are reverified. A newer
stable release is tested before publication so an already-fixed older-nightly
failure does not become a new pre-stable regression issue.

## Authentication and GitHub

Scout uses Wendy's documented **admin entitlement**, which mounts the local
Agent Unix socket. The trusted supervisor uses canonical generated API messages
to confirm Development enrollment and uses each shipped CLI's supported
`WENDY_AGENT_SOCKET` path. It discovers its own device without copying operator
profiles, requesting a browser login, reading device private-key persistence,
creating a service account or weakening network TLS. The local socket is a
powerful grant: **full device control is available to trusted Scout supervisor
code**. Install Scout only when you trust that code. The model/worker never
receives this mount or an arbitrary Agent RPC/shell interface.

The publisher needs a **new fine-grained PAT** restricted to the one configured
repository: **Issues read/write and Metadata read-only**, or an equivalently
scoped installation token. Default repository: `wendylabsinc/WendyOS`; an operator
may set `SCOUT_ISSUE_REPOSITORY` to one explicitly allowed repository. Public
context/artifact reads still use the fixed official WendyOS sources.

Provision the token through your trusted device-management path as:

```text
control container: /state/secrets/github-issues-token
owner: root
mode: 0600
```

This is the `wendy-scout` persist volume's `secrets/github-issues-token` file.
Keep it out of images, source, command arguments, environment variables, logs
and the worker volume. Never use `gh auth token` or copy an existing personal
Mac profile. A token awaiting organization approval is not usable publication
authority. Scout retains findings when the file is missing or the API refuses
it. Lack of a credential is reported separately from testing; it does not prove
standalone live publication. No canary issue is created.

Only this publisher can make GitHub writes. It allows issue creation and
idempotent creation of `ai-found`, no comments, discussions, mentions, workflow
runs, repository code writes, Slack or email. Publication is capped at two
attempts per day and one per hour. It scans open **and closed** issues, records
fingerprints and ambiguous write attempts durably, and holds if duplicate
coverage is incomplete. Issue text is generated from typed, replayed command
observations. Model prose and arbitrary script stdout are never posted.

## Experiments and evidence

Pi sees fixed local-self probes (info, enumeration, help and owned scratch
lifecycle), bounded Python script tools and deterministic replay requests.
Offline scripts can execute the verified CLI binaries, compose parsing and
argument experiments, and analyze fresh projected probe observations from
`inputs.json`. They may print or modify the precreated `output.txt` with `r+`.
File creation/package installation and network access are unavailable in the
script sandbox. A fresh nonce/arithmetic script-result proof gates sessions.
Previous model summaries are marked historical and untrusted, not fresh evidence.

The replay boundary supports exit regressions, invalid JSON, stable device-info
field changes/removal, missing help flags and owned lifecycle checks. It repeats
baseline/nightly tests and checks current stable. A baseline-working command
removed or made unsupported in a later nightly remains a candidate regression.
Shared setup/auth failures do not establish one. Arbitrary script assertions
remain exploratory evidence until a trusted supported replay substantiates them.
Agent cycling and unrestricted live CLI arguments are deliberately unavailable.

Inspect normal Wendy logs where supported:

```sh
wendy --json --device <your-dev-thor> device logs sh.wendy.scout --tail 100 --no-follow
```

The durable files are authoritative if Agent log replay omits stdout. Through a
trusted operator shell, inspect the corresponding persist-volume files:

```text
wendy-scout/events.jsonl            startup, GPU and session lifecycle
wendy-scout/model.log[.1]           bounded model-server logs
wendy-scout/release-pins.json       exact verified CLI releases/digests
wendy-scout/runs/<UTC>/            context, versions, projected and private raw probes
wendy-scout/findings/               verified findings retained independently of run pruning
wendy-scout/publisher.json          deduplication and issue-rate ledger
wendy-scout-work/preflight.json     live kernel/namespace/cleanup checks
wendy-scout-work/bootstrap-complete.json   positive offline CLI checks
wendy-scout-work/runs/              bounded Pi events, scripts and summaries
```

The host's normal volume root is `/var/lib/wendy/volumes/`; use `wendy device
volumes list` to confirm it. Raw `*.private.json` files can contain identifying
device output: keep them local. Public/model projections retain diagnostic
versions, architecture, counts/status and sanitized errors, excluding device
IDs, camera serial paths, network addresses and credentials.

## Enforced isolation and limits

The worker has its own network/PID namespaces with only loopback, no external
route, admin socket, device mounts or publisher credentials. It communicates
with control through a narrow root-owned Unix socket bridge. Pi's built-in
tools, ambient extensions, skills, context files, MCP discovery, package updates
and telemetry are disabled. Only the packaged extension is loaded.

Offline test children run as a separate UID with `no_new_privs`, Landlock ABI
3+ filesystem restrictions and an inherited seccomp allowlist. `/proc`, `/ipc`,
other sessions and secrets are outside their readable view. Socket creation,
ptrace/process-memory access, namespace/mount changes, process-group escape,
new files, links and executable-memory file handles are denied. Only precreated
bounded output is writable. File descriptors are closed before exec. Tests have
wall/CPU/address-space/PID/file/output limits, and the supervisor kills and reaps
the whole process group even if its leader exits first. No `build`, privileged
container or `CAP_SYS_ADMIN` grant is used. A failed positive or negative kernel
preflight holds testing rather than falling back to UID/environment separation.

Each Pi session has a 270-second wall deadline, model/tool-call budgets and a
bounded event stream. Session directories and logs are retained with caps;
verified findings are retained separately before publication or run pruning.
Worker loss cancels testing and scratch leases; group shutdown terminates all
service children. The native Pi/model libraries, Linux kernel, Wendy Agent,
trusted supervisor and device administrator remain trusted components.

These controls address the writable shared cache/indirect-network problems in
[OpenAI's Hugging Face incident report](https://openai.com/index/hugging-face-incident-and-the-road-ahead/)
and unintended internet/package publication and scope drift described in
[Anthropic's assessment](https://www.anthropic.com/research/alignment-assessment-cybersecurity-incidents).
The latter explicitly did not involve multi-agent coordination. Repository and
issue text is data, never authorization. Structured issue content/rate limits
reduce GitHub's use as a cross-Scout message board, but **do not eliminate covert
channels through authorized text, choices or timing**. This is defense in depth,
not a claim of perfect containment or protection from kernel/native-code flaws.

## Tests and bounded validation

```sh
python3 -m unittest discover -s tests -v
```

Tests exercise inference fetch/path rejection, private-data projection, malformed
and already-fixed replay cases, all-state duplicates, rate/route restrictions,
and label/issue creation against a **local fake HTTP server**. No test posts a
live GitHub canary. The canonical protobuf revision/file hashes are recorded in
`control/canonical.json`; image generation downloads and verifies those exact
repository schemas and uses `protoc`, without maintaining a parallel wire schema.

For a kernel-only deployment, add `--env SCOUT_BOOTSTRAP=1`. It performs namespace,
positive Python/Go CLI, negative syscall/filesystem and descendant-cleanup checks
without a model session. For a bounded exploratory deployment use
`--env SCOUT_MAX_SESSIONS=2`. Default operation continues supervised sessions;
stop the Wendy app to stop testing. The normal runtime does not change Agent/OS.
