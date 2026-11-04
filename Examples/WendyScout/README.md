# Wendy Scout PoC

**Autonomous bug hunting on your Wendy device.**

One Wendy app runs a pinned local GPU model and a fresh Pi session per iteration.
Pi uses recent public commits/patches/releases/issues and recent experiment history
to choose one functional hypothesis and stable/nightly workflow pair per session.
Trusted tools reset, exercise and remove only
Scout's disposable app. They return exact commands, expected/actual checks and
fresh evidence IDs. Optional offline scripts help diagnose results. Help/version
output and arbitrary changed JSON values cannot produce an issue.

## Run

From this directory, use a Development-enrolled ARM64 Thor with its builder enabled:

```sh
wendy --json --device <dev-thor> run --build-host <dev-thor> --detach --yes
wendy --json --device <dev-thor> device apps list
wendy --json --device <dev-thor> device logs sh.wendy.scout --tail 100 --no-follow
wendy --json --device <dev-thor> device apps stop sh.wendy.scout
wendy --json --device <dev-thor> device apps start sh.wendy.scout --detach
```

Stop the bare app ID to stop control, worker and test children. The fixed scratch
workload exits when its supervisor lease disappears; restart removes only recorded
owned leftovers. Weights, tools and evidence persist. A new session starts after a
60-second pause. Context/releases are cached for 30 minutes and each session pins
verified CLI artifacts. `--env SCOUT_MAX_SESSIONS=3` is a worker validation cap;
normal operation has no cap. Missing fresh functional work or failed cleanup holds
testing. Model narration alone never establishes execution.

## Functional experiments

| Workflow | Fixed expected behavior |
| --- | --- |
| `restart_persistence` | Start and observe readiness/RUNNING, stop/STOPPED, restart with the retained counter incremented, then stop/remove/ABSENT. |
| `reject_extra_args` | Reject an extra stop argument without changing a running app or its persistent data; normal stop/remove still succeeds. |
| `logs_output` | Observe attached readiness output and bounded stored logs for the same owned fixture; stop/remove cleanly. |

CLI list state is checked against the canonical local Agent API. The fixture uses
only the dedicated `wendy-scout-fixture` volume, mounted in trusted control and the
fixed scratch workload, never the model/test worker. Each experiment resets only
its owned files and app. Baseline/nightly replay uses identical fixture state and
image. A suspected failure must occur twice in nightly with matching failed checks,
pass twice on the baseline, and have a conclusive current-stable contrast.
Shared failures, blocked setup, flaky results, already-fixed behavior and old-Agent
limitations remain local evidence. A real regression is deduplicated across open
and closed GitHub issues before an `ai-found` issue with projected evidence/repro.
No comments, recurrence accounting, coordinator or automatic failed-session retry.

For deterministic operator replay without racing model sessions, redeploy with
`--env SCOUT_BOOTSTRAP=1`, then use the supported operator shell:

```sh
wendy --json --device <dev-thor> device shell -- nerdctl --namespace default exec sh.wendy.scout_control python3 /opt/scout/reproduce.py restart_persistence stable
```

The helper accepts only the three fixed workflows and `past`/`stable` baselines;
it refuses normal model-running mode and never publishes. Redeploy normally to
resume autonomous testing. Bootstrap also performs kernel/offline-CLI preflight.

## Live transcript

Open `http://<thor-lan-ip>:8087/`, or Companion Mesh VPN → Development device →
Apps → Scout → Open. The existing Wendy HTTP entitlement provides that route;
physical Mesh/iPhone browsing remains untested. Only read-only GET `/` is served.
The escaped/redacted native HTML page refreshes every five seconds and reads the
same bounded `events.jsonl` appended/flushed during execution. It shows unverified
model hypotheses, actual workflow calls/results and expected/actual checks. Hidden
reasoning, credentials and raw private control probes are excluded. No controls,
history browser, arbitrary files or download API. Stopping the app stops the page.

## Model and limits

Pins select llama.cpp-maintainer Qwen3.8-Flash-Next IQ4_NL, **102.05 GB in two shards**,
verified by immutable revision/size/SHA-256. The existing official ARM64 CUDA 13
image contains upstream llama.cpp `39febd2adb947b70da35e95f9351b3bf58ad170a`.
There is no remote model fallback, vision/MTP loading or host tool installation.
The text model endpoint binds host loopback 18081. Startup requires available RAM
for weight bytes plus 12 GiB, with a separate bounded 900-second load window.
Startup uses 512/128 logical/physical batches. A later load exceeded the original
600-second window during native GPU allocations despite available RAM; startup
failure holds testing and terminates the loader. This is a runtime limitation,
not a CLI regression.

Actual unchanged-prompt trial: 49/49 GPU layers, 69,200.9 MiB GPU plus 28,110.1 MiB
host weights, 170.34s GPU health, about 21 decoded tokens/s. Three turns performed
real tools with proof (184.76s/188.84s/198.54s). Two finished normally; the third
filled 16K context (16,331 prompt tokens left only 53 output tokens). That was a
context limit, not proof of model weakness. Invalid report arguments and one stale
chronology claim remained model mistakes; no regression was accepted. The functional
path replaces that help-heavy prompt and results with compact trusted workflows.
Check rows preserve expected/actual values without repeated labels/commands;
full ordered traces remain durable. A six-workflow functional turn also exceeded
16K, so the prompt finishes after one comparison rather than exhausting the menu.
The cached 30B rollback pins remain in both service directories at `29ede24d5`.

Direct Thor replay passed all three functional workflows twice on stable and
nightly with checked cleanup (about 0.55–0.85 seconds per experiment). Stopping
Scout stopped the fixture; restart removed its recorded leftover. These trusted
operator checks do not by themselves establish autonomous model reliability.

The worker has a 270-second Pi-loop deadline; control has a 300-second supervisor
window and each probe batch is additionally bounded to 90 seconds and the remaining
window. Flash has fixed non-thinking sampling and at most 4096 output tokens; model
requests cannot override backend/template settings. Worker memory/tasks, script
CPU/wall/PID/file/output limits and bounded evidence retention remain enforced.

## Credentials and containment

Control's documented `admin` entitlement mounts `/run/wendy/agent/agent.sock`:
**full device control for trusted supervisor code**. Model tools expose only fixed
Development/self operations on the owned fixture. Agent/OS/power changes, other
apps/devices, physical capture/motion and arbitrary host shell/RPC are unavailable.
No browser login, copied Mac profile/private key or service-account key is needed.

The worker has network:none, separate PID/network namespaces, no admin/secret/fixture
mounts and separate model/script UIDs. Offline scripts additionally use Landlock
ABI 3+, seccomp, closed FDs and no-new-privileges; sockets, proc/secret access,
ptrace, namespace/group escape and arbitrary file creation are denied. Children are
killed/reaped and a failed live preflight holds testing. Built-ins, ambient org
extensions/context, MCP discovery and cloud providers are disabled. Trusted root
supervisors, Pi/model native libraries, Agent and kernel remain trusted; this is
not protection from compromised trusted code or all covert authorized issue text.

Publication needs a new fine-grained PAT restricted to the configured repository's
Issues read/write and Metadata read-only. Default: `wendylabsinc/WendyOS`;
`--env SCOUT_ISSUE_REPOSITORY=<owner/repo>` selects an explicitly scoped alternative.
Public context remains WendyOS; private duplicate reads use that scoped token.
After any organization approval, transfer a new local mode-0600 file:

```sh
python3 provision_token.py --device <explicit-dev-cloud-selector> --file <new-scoped-token-file>
```

The reviewed operator helper disables PTY echo before reading/sending the token.
Only control persist stores it at `/state/secrets/github-issues-token`, root0600.
Keep it out of source/images/argv/environment/logs. Without usable auth, findings
stay local. Earlier private synthetic transport testing proved issue HTTP201 and
an authenticated duplicate retry without another POST; it was not a discovered bug.
New functional publisher fixtures use local fake transport, never fabricated live bugs.

Evidence lives in `wendy-scout`, `wendy-scout-work` and `wendy-scout-ipc` volumes
under the normal host root `/var/lib/wendy/volumes/`. Raw `*.private.json` stays local.
Only typed workflow checks, fixed commands, versions and safe digests reach issues.
Canonical protobuf clients are generated from pinned repository schemas, not a
parallel wire contract.

```sh
python3 -m unittest discover -s tests -v
```
