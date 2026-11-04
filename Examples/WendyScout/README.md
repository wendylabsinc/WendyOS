# Wendy Scout PoC

**Autonomous bug hunting on your Wendy device.**

One Wendy app runs a pinned local GPU model and Pi. One loop gives each fresh Pi
session the same prompt, recent public WendyOS commits/releases/issues, and
previous evidence. Pi writes bounded offline CLI experiments and uses fixed
local-device probes. A suspected regression is replayed on stable/nightly CLI
versions on the same Agent. Scout checks existing open/closed issues and creates
an `ai-found` issue with exact versions, commands and projected evidence when the
finding is verified. A no-bug or blocked result is useful.

## Run

Use a Development-enrolled ARM64 Thor with its Wendy builder enabled:

```sh
wendy --json --device <dev-thor> run --build-host <dev-thor> --detach --yes
wendy --json --device <dev-thor> device apps list
wendy --json --device <dev-thor> device logs sh.wendy.scout --tail 100 --no-follow
wendy --json --device <dev-thor> device apps stop sh.wendy.scout
wendy --json --device <dev-thor> device apps start sh.wendy.scout --detach
```

`sh.wendy.scout` contains trusted control and isolated worker services. Stop the
bare app ID to stop both and their test children. Weights and evidence persist
across restart/redeploy. The first start downloads the official 56.7 GB Qwen3-Coder-Next Q5_K_M GGUF
(four shards) on-device;
`control/pins.json` pins its revision/digest, official ARM64 CUDA base image and
Pi 0.73.1. GPU offload is required. There is one local model provider and no
cloud-model fallback. The model server uses host loopback port 18081.

The earlier 18.6 GB model cache is retained. To roll back, restore both
`control/pins.json` and `worker/pins.json` from commit `0a917472b`, then redeploy;
the single-file loader still works and reuses that cached GGUF.

Normal operation starts a fresh bounded session, retains its evidence, waits
60 seconds, and repeats until the app stops. Public context/releases are cached
for 30 minutes; each session pins its CLI artifacts and verifies their digests.
For operator validation, `--env SCOUT_MAX_SESSIONS=1` stops further sessions after
one result while keeping the worker alive. `--env SCOUT_BOOTSTRAP=1` performs
kernel/offline-CLI checks without loading the model.

## Live transcript

The app declares one read-only HTTP page on port **8087**. Open
`http://<thor-lan-ip>:8087/` on a reachable LAN, or in Companion enable the
existing Wendy Mesh VPN, select the Development device, then Apps → Scout → Open.
The HTTP entitlement supplies that existing Open route; no mesh transport is
implemented here. Companion Mesh browser access must be tested on the actual
client; it is not implied by a successful local HTTP check.

The page refreshes every five seconds and shows the latest bounded transcript,
session/time/state, visible unverified model narration, submitted scripts and
redacted worker results. `events.jsonl` is appended and flushed during the run,
including before `result.json` exists; interruption preserves completed events.
Hidden reasoning and raw private control probes are excluded from the page.
Only GET `/` is served, with no control buttons or file/download routes. Stopping
the app makes its page unreachable. An absent heartbeat is marked stale.

## Credentials

The control service uses the documented `admin` entitlement and local Agent Unix
socket. This is **full device control for trusted supervisor code**. Its tools
permit only this Development device, enumeration/info/help and one test-owned
scratch app. It needs no browser sign-in, copied operator profile or service
account. It preserves Agent/OS and power mode; physical capture/motion and
Agent/OS updates are unavailable to the model.

Issue publication needs a **new fine-grained PAT**, limited to the configured
repository's **Issues read/write and Metadata read-only**. Default repository:
`wendylabsinc/WendyOS`. An explicitly scoped alternative can be selected at run
time with `--env SCOUT_ISSUE_REPOSITORY=<owner/repo>`; public context remains
WendyOS. Private duplicate reads use the publisher's scoped token.

After any required organization approval, transfer a new local mode-0600 file:

```sh
python3 provision_token.py --device <explicit-dev-cloud-selector> --file <new-scoped-token-file>
```

The operator helper checks Development identity and disables receiver PTY echo
before reading/sending the file. It stores the token only in control's persist
volume at `/state/secrets/github-issues-token`, root-owned mode 0600. Keep it out
of source, image contexts, argv/environment and logs. Never copy an existing Mac
OAuth/profile. Without usable publication auth, evidence remains local.

## Tools and isolation

Pi has only the packaged probe, script and report tools. Offline Python can run
verified CLI binaries and analyze `inputs.json`; only precreated `output.txt`
can be written using `r+`. Authenticated Wendy operations stay behind fixed
supervisor commands; Pi gets no host shell or arbitrary Agent RPC/HTTP tool.

The worker has network:none, separate PID/network namespaces, no admin/secret
mounts, separate users and a 2 GiB/128-task limit. Script children additionally
use Landlock ABI 3+, seccomp, no-new-privileges, closed FDs and CPU/wall/PID/file/
output limits. Socket creation, proc/secret access, ptrace, namespace changes,
file creation and process-group escape are denied. Descendants are killed and
reaped. A failed live preflight holds testing. Pi's built-ins, ambient extensions,
context files, MCP discovery and cloud providers are disabled.

One session has a 270-second deadline and fixed model/tool-call budgets. Logs,
sessions and retained findings have size/count caps. Trusted root supervisors,
Pi/model native libraries, Agent and kernel remain trusted. These controls do
not claim protection from compromised trusted code or eliminate covert channels
through authorized issue choices/content/timing. Untrusted issue/source text
cannot authorize new tools or network access.

## Evidence and known limits

Persistent volumes are `wendy-scout` (model/tools/private raw probes/findings),
`wendy-scout-work` (Pi runs/scripts/preflight) and `wendy-scout-ipc` (root-only
control socket). Inspect `events.jsonl`, `model.log`, `release-pins.json`,
`runs/` and `findings/` through trusted operator access. Normal host volume root:
`/var/lib/wendy/volumes/`. Agent log replay can omit stdout; durable files are
authoritative. Raw `*.private.json` stays local; published observations exclude
identifiers, addresses, camera serials and arbitrary script/model prose.

Tested on an older Agent `2026.10.29-210857`, WendyOS `0.17.0`, CUDA `13.2`,
`sm_110`: 49/49 GPU layers; actual model-authored offline CLI execution and
follow-up; info/camera/audio enumeration and scratch lifecycle with past/current
stable and nightly; kernel/FD/egress/resource/descendant checks; stop and forced
controller recovery. Cached weight hashing took 16.45–16.50 seconds and cold GPU
health 39–40 seconds. A fresh 30B PoC session took 74.19 seconds.

The official Qwen3-Coder-Next Q5_K_M trial used the same prompt/tools, 16K context
and CUDA image. All four shards passed their pinned hashes; download, verification
and one rolling deployment took 9m55s (about 95 MB/s end to end). The existing
server recognized `qwen3next`, offloaded 49/49 layers and used a 53,873.5 MiB GPU
model buffer. Cold GPU health took 92.16 seconds, with about 68 GB RAM available
at the startup snapshot. One fresh session completed in 140.04 seconds with
actual tool/result proof. It ultimately called additive help text benign, but
also tried unsupported report fields that the guard rejected. No regression was
accepted. This single slower session demonstrates compatibility, not a reliable
quality or benchmark improvement. The normal loop uses Next; rollback remains
available above.

The original 30B model mistook additive help text for a regression; trusted
checks accepted no finding. Replay covers a small set of exit/JSON/info-field/help predicates;
arbitrary script assertions remain exploratory. Earlier setup exposed Pi API
compatibility, verbose partial events and context overflow; the current runtime
uses pinned compatibility, completed-event capture and bounded context. An older
Agent can leave stopped scratch metadata during group stop; restart cleans only
the recorded owned resource.

A separately authorized synthetic test in a temporary private repository proved
Thor issue creation (HTTP 201) and an authenticated duplicate retry with no second
POST. The fixture was explicitly **not a bug**, separate from model tools, and
production verification was retained. Credential checks confirmed root-only
storage, no worker mount and denied untrusted-UID socket/proc access. This proves
the exercised operations, not every token permission. No real regression issue
has been demonstrated. The product default remains WendyOS.

```sh
python3 -m unittest discover -s tests -v
```

Tests cover the inference/credential boundary, projected privacy, replay
truthfulness, existing-issue lookup, oversized evidence and secure transfer.
Canonical protobufs are generated from the pinned repository schemas in
`control/canonical.json`, without a parallel wire contract.
