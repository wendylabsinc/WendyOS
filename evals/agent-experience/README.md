# Wendy agent experience benchmark

Measure whether Codex, Claude Code, and Wendy chat can finish the same Wendy task
from one prompt. Report each agent and each target separately. A simulator pass
does not count as a hardware pass.

The runner uses Python's standard library on macOS or Linux. It launches the real agent CLIs, saves
their JSON event streams, and runs a separate verifier against the target. It
never sends a follow-up prompt or answers an approval request.

## Run

Build the candidate CLI and copy the example configuration:

```sh
make -C go build-cli
cp evals/agent-experience/config.example.json evals/agent-experience/experiment.local.json
```

In that file, set `wendy` to the candidate binary's absolute path. Set each model
to its configured value for an experience benchmark, or to a pinned model for a
controlled comparison. Record reasoning effort and any overrides alongside the
agent entry. Credentials stay in the existing CLI logins or provider environment
variables. Do not put keys in this file; the runner saves a configuration copy.

By default, each trial gets a private Wendy device/cloud configuration and VM
store through `WENDY_CONFIG_DIR`. The candidate must support that variable; the
runner checks it before execution. Model settings and coding-agent logins stay
configured on the host. The simulator target in the config names a result group;
app tasks get a fresh private VM during unscored preparation. Set `agent_binary`
to the matching Linux ARM64 agent to update that fixture before timing starts.
Simulator-creation tasks make their own VM during the timed LLM attempt instead.
A cloud target must use a complete selector:

```text
cloud://HOST:443/org/ORG_ID/asset/ASSET_ID
cloud://HOST:443/tenant/TENANT_UUID/asset/ASSET_UUID
```

The selector must match the endpoint and organization of a saved login. A display
name or a forwarded local port is insufficient to establish cloud transport.
The target needs container exec access for an independent HTTP probe through
the selected transport. The probe runs Python inside the exact app container.
Reserve port 18765 and the adjacent worker port 18766 for Compose, or set another `port`.

Preview a six-attempt pilot without calling any model or device:

```sh
python3 evals/agent-experience/benchmark.py \
  --config evals/agent-experience/experiment.local.json \
  --task deploy-http --repetitions 1
```

Add `--execute` to run it. Omit `--task` for the full suite. `--agent`, `--target`,
and `--task` can be repeated. `--seed` controls the shuffled execution order.
`--limit` limits the number of planned attempts. Default repetition count is three;
use ten or more per task and agent when estimating reliability. One pilot attempt
checks the benchmark plumbing and is not a success-rate estimate.

The example grants the agent the tools needed for the task before the attempt.
Codex keeps its workspace sandbox, enables network access, and grants the private
Wendy state and cache directories needed for VM/runtime creation. The cache is
`~/Library/Caches/wendy` on macOS or `$XDG_CACHE_HOME/wendy` on Linux, falling back
to `~/.cache/wendy`. Wendy also uses `~/.cache/wendy` for build locks and Docker
configuration on macOS. Docker's own `buildx` state directory needs write access
because Wendy links to it. The example grants these specific directories too.
Claude's MCP process receives the same private state setting. Claude gets explicit
tool permissions; Wendy chat uses `--yes`. Permission denials remain observable
failures or recovery work. These are different product permission mechanisms,
so record them as part of the configuration.

## Tasks and grading

| Task | Starting state | Required outcome |
| --- | --- | --- |
| `create-simulator` | Unique VM name does not exist | Named VM is running and its WendyOS agent reports a version |
| `deploy-http` | Working app source, nothing deployed under its unique ID | Correct app is running; two HTTP checks return its nonce, ID, and version |
| `repair-startup` | App deployed with a reproduced configuration-key crash | Agent fixes the source and redeploys; the original app ID serves correct HTTP output |
| `update-http` | Version v1 is independently verified | The same app serves v2 and preserves its nonce and port |
| `stop-app` | App is independently verified running | App remains deployed in STOPPED state on two checks |
| `start-app` | App is deployed, healthy, then independently stopped | Existing app runs again and passes two HTTP checks |
| `compose-app` | Requirements only; no implementation or Compose file | Agent creates two services; fresh requests traverse the worker; frontend fails with 503 when the verifier stops the worker and recovers after restart |
| `create-go2-simulator` | Unique VM name does not exist | New Go2 VM and pinned runtime are healthy; isolated DDS supplies fresh odometry and 12 distinct joints |
| `create-g1-simulator` | Unique VM name does not exist | New G1 VM and pinned runtime are healthy; isolated DDS supplies fresh odometry and 29 distinct joints |
| `debug-otel-logs` | Real OTLP-instrumented app with a reproduced division-by-zero fault | Agent queries its diagnostic logs, repairs it, and a new independent workload succeeds with instrumentation retained |
| `debug-otel-metrics` | Real instrumented worker exceeds its latency limit | Agent queries its metric series and fixes the cause; fresh workload latency and traces meet the limit |
| `debug-otel-traces` | Worker spans expose a reproduced latency fault | Agent queries correlated spans and repairs the worker; independent new root/child spans verify recovery |
| `debug-go2-ros2`, `debug-g1-ros2` | Healthy robot in a new VM; supplied consumer subscribes to a missing topic | Consumer configuration is repaired; unchanged consumer code receives fresh robot odometry with correct frames |
| `repair-go2-camera`, `repair-g1-camera` | Healthy robot in a new VM; virtual camera is disabled and absence of fresh frames is reproduced | Camera delivers fresh correctly encoded RGB frames and responds to independent changes in the scene |

All app tasks support both real WendyOS VMs and cloud hardware. The VM
uses the real agent, container runtime, builds, and networking. These tasks do
not mock successful device responses. They exercise simulator lifecycle,
deployment, diagnosis, updates, and shutdown. They do not establish camera, GPU,
physical sensor, or robot-motion parity. The Unitree creation tasks check actual
ROS messages, increasing capture timestamps, frame IDs, finite values, and normalized
orientation. They do not establish factory gait, sensor calibration, or audio parity.

The catalog also defines physical installation, OS OTA, ROS 2 diagnosis, camera
capture and repair, audio loopback and peripheral I/O. These seven tasks require external lab fixtures and independent
verifiers that have not yet been implemented here. Their acceptance contracts
are in `tasks.json`; see [fixture setup](fixtures.md). The runner reports them as
`unconfigured` until a target supplies all required fixture commands and capabilities.
They are never counted as passes or silently omitted from coverage.

The OTEL app exports real HTTP/protobuf records into Wendy's collector. The grader
checks the model's returned tool evidence, then generates a new random request
challenge and collects logs, metrics and correlated spans itself. Successful
responses alone do not pass. The deployed exporter must match the fixture's
immutable source. These first cases use a Python worker within one process;
they do not measure distributed tracing across a production service mesh.

Go2 and G1 creation each use a new VM. The image may contain an older agent;
provide a compatible candidate ARM64 agent binary as an initial environment fact
using `environment_context` in the run configuration. The LLM is responsible for
any required update during its timed attempt. Record the binary hash in the config.
This is separate from OS OTA, which must change and commit the actual OS artifact.

Every attempt gets a fresh working directory, transcript, app ID, and response
nonce. Setup captures device version and the state of unrelated apps. Verification
checks that those apps retain their prior version and running state. Cleanup
removes only the attempt's app and image, or its uniquely named simulator. Failed
cleanup stops the experiment so later attempts cannot inherit it.

Setup for repair/update/stop uses deterministic commands outside the timed task.
It checks the starting condition before handing control to the agent. A setup
failure is an infrastructure result, excluded from agent success-rate denominators
and counted separately. Agent failures, timeouts, early exits, and failed outcome
checks remain in the denominator. A final "done" message or a zero process exit
code alone never establishes success.

## Measurements

- Unassisted success means verified outcome, a completed agent turn, and zero
  human messages or approvals after the initial prompt. The runner enforces zero
  human input. It does not silently rescue a stopped or confused agent.
- Task time starts at agent launch and ends after independent verification.
  Raw results also separate agent time, setup, verification, and cleanup. It is
  not a continuously sampled time-to-first-health measurement.
- Token totals include input and output across the attempt. Cached input and
  cache writes are subsets of normalized input. Reasoning is a subset of output,
  so neither gets added twice. Wendy emits usage for each request, including local
  delegated requests. Claude's final session total replaces per-message totals.
- Report tokens per attempt and total tokens across successes and failures divided
  by verified successes. A cheap failure cannot improve cost per success.
- Missing or partial usage is unknown, never zero. Aggregate token statistics
  remain unknown if any included attempt has incomplete accounting. Timeouts may
  incur provider charges that the final event stream never reports.
- Tool calls, model names when exposed, permission denials when exposed, raw logs,
  and final text help explain failures. Tool event granularity differs by client,
  so tool-call counts are diagnostic rather than a cross-agent quality score.

`results/<experiment>/summary.md` compares each agent, target, and task.
`summary.json` includes p90 successful time, timeout counts, setup failures, and
usage coverage, plus estimated dollars per attempt and per success including
failures. `coverage.json` also lists unconfigured cases, setup errors and attempts
not run for every agent. `results.jsonl` has one full record per attempt. Each attempt
directory retains the prompt, command arguments, stdout, stderr, and verifier
evidence. Temporary workspaces remain at the recorded paths for diagnosis.

The `prices_per_million` fields are `input`, `cached_input`, `cache_write`,
and `output`. Fill them for Codex and Wendy before running multiple paid attempts;
their event streams do not supply a dollar total. Supply a source and capture date. Rates must match the experiment's
models, service tier, and context length. With mixed models, use a documented
conservative rate for budgeting or calculate exact costs from the raw per-model
records later. Claude's reported dollar figure is a client estimate. API-equivalent
token costs are not subscription charges.

`budget_usd` and `trial_reserve_usd` implement a budget check between attempts.
Set `prior_spend_usd` to carry spending forward when splitting one authorization
across experiments. Reserve a conservative allowance for interrupted attempts
whose final usage is unavailable; do not reset the authorization for each file.
The example reserves $15 before each run and caps Claude's own run budget at $15.
If no usable cost is reported or calculated, the runner stops before starting
another paid attempt. This is a soft experiment budget: Codex only reports usage
at turn completion, so an in-flight attempt can exceed its reserve. Use provider
spend limits for a hard billing cap. Wall-time limits terminate the process group
and retain partial logs.
`max_agent_seconds` can cap the task-specific timeout; the plan and each result
record the effective limit. The example caps attempts at 900 seconds.
`syntax_error_limit`, default 3, stops an attempt when that many distinct completed
tools return the same CLI command/flag syntax error. This includes errors hidden
by a shell pipeline that exits zero. The guard reads returned tool output only;
it does not send hints or count model prose as a failure. Connection failures and
normal VM boot waits do not trigger this rule. The result is `no_progress`, with
incomplete usage retained as unknown. This changes the stopping policy for future
trials; earlier results and their original limits remain unchanged.

## Keep comparisons fair

The default measures the configured experience on this host. It preserves host
settings and existing logins, supplies Wendy's runtime skills to Codex and Claude,
and disables Wendy chat memory. Wendy device state is isolated. Cloud and direct physical targets
receive a private copy of the existing cloud auth fields. Credentials remain
outside app build contexts and are removed after cleanup; device pins and host
VMs are not copied. `WENDY_SECRET_STORE=file` keeps refreshed trial credentials
in the private directory without writing the host Keychain. A watchdog stops an attempt if existing host device pins change.
It retains that failed result for review and never silently changes it to a pass.
Codex and Claude can still load personal settings,
global instructions, hooks, and other configured context. A fresh transcript is
not an isolated user profile. Record that context policy; use a dedicated OS user
or isolated runner for published comparisons. Do not run grading files inside an
agent's editable workspace. Local process isolation here is not an adversarial
security boundary.

Freeze prompts, CLI and plugin revisions, agent versions, model and effort,
permissions, device OS/agent versions, host resources, transport, and cache policy.
The manifest records versions, configuration, and task hashes; setup logs capture
target information. The manifest also hashes the CLI binary, runner, and fixtures, and saves the chat/config source and diff.
It copies fixture scripts and app source. Robot verification reads passive runtime
status before connecting to the device, because a Wendy device connection can
automatically provision a missing robot runtime. A grader must never complete the
LLM's unfinished work.
Image and build caches are shared unless the operator resets them. Label cold and
warm runs separately. Do not flush a shared physical device's caches as an
implicit part of a benchmark.

Compare Wendy versions with the same agent/model settings. Compare products in
separate rows. Randomize order, serialize access to each device, and retain failed
runs. Review failed transcripts for missing capabilities, tool errors, permission
blocks, wrong target selection, premature completion, and requests for steering.
If a human intervenes during a diagnostic rerun, record it as a separate assisted
run; never replace the original unassisted result.

## Verify the benchmark implementation

```sh
python3 -m unittest discover -s evals/agent-experience -v
go test ./go/internal/cli/chat
```

The automated tests cover accounting semantics, incomplete usage, failed-attempt
costs, process timeouts, provider streams, and runner orchestration. Fake-agent
test results are not model performance results.

Before using a new target, check each task's fixture with deterministic commands.
The runner can save these checks as calibration evidence without calling a model:

```sh
python3 evals/agent-experience/benchmark.py \
  --config evals/agent-experience/experiment.local.json --target simulator \
  --calibrate --task debug-otel-logs --task repair-go2-camera --execute
```

Calibration output records `model_calls: 0`, stdout, stderr, elapsed time and
cleanup separately. It is never included in the agent performance summary.

Run from a fresh scratch directory, supply the absolute CLI path, and use a unique
twelve-character lowercase hexadecimal run ID:

```sh
python3 /path/to/wendyos/evals/agent-experience/fixtures.py \
  smoke deploy-http --wendy /path/to/wendyos/go/bin/wendy \
  --device vm:wendy-eval-pilot --run-id a1b2c3d4e5f6 --port 18765
```

Repeat for `repair-startup`, `update-http`, `stop-app`, `start-app`, `compose-app`,
`create-simulator`, `create-go2-simulator`, and `create-g1-simulator`.
Robot smoke checks accept `--agent-binary /absolute/path/to/wendy-agent-linux-arm64`.
The robot diagnosis fixtures need `make -C go build-agent-linux-arm64` too. They
prepare their own robot VM before starting the timed LLM attempt. Calibrate them
with `robot_diagnostics.py smoke go2 ros2`, `smoke g1 ros2`, `smoke go2 camera`, and
`smoke g1 camera`, supplying the CLI path, agent binary, run ID and port.
OTEL smoke checks use `otel_fixture.py smoke logs`, `smoke metrics`, or `smoke traces`
with the same `--wendy`, `--device`, `--run-id` and `--port` arguments.
These checks make real temporary apps or VMs and clean them up. They use no model
tokens and must run outside measured attempts to avoid resource contention.

The [initial deployment pilot](pilot-2026-09-27.md) records six live attempts,
one per agent and target. The [robot and OTEL fixture calibration](fixture-calibration-2026-09-27.md)
records the additional checks that made no LLM calls.

Codex event details are documented in [OpenAI's non-interactive mode guide](https://developers.openai.com/codex/noninteractive/).
Claude's stream and cost fields are documented in [its programmatic usage guide](https://code.claude.com/docs/en/headless).
Anthropic's [streaming guide](https://platform.claude.com/docs/en/build-with-claude/streaming)
describes cumulative output usage. The older
[device-to-robot scenarios](../../plugins/wendy-agentic-coding/evals/device-to-robot.md)
are candidates for additional tasks with their own fixtures and verifiers.

The [robot simulator pilot](robot-pilot-2026-09-27.md) records four unassisted passes,
one interrupted Claude attempt, and the remaining unrun case.

The [CLI syntax regression check](cli-syntax-regression-2026-09-27.json) reproduces
the misleading ROS2 error without contacting a device. The corrected CLI reports
the bundled device argument and a valid invocation. The retry guard was also
replayed against the saved Claude transcript. These are regression checks, not
evidence that a model now recovers unassisted.
