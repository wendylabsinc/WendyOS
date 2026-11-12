# Spare compute worker

Run bounded Go fuzzing, mutation testing, shuffled race tests, and local Qwen code
reviews while a self-hosted CI machine is idle. The worker runs as UID 10001,
without a Docker socket, host checkout, credentials, or device access. The optional
NVIDIA model server and review worker run separately, so GPU inference can
continue while the CPU worker tests. Nothing creates issues, sends messages, edits
the source repository, or applies model suggestions.

The CPU worker builds for Linux amd64 and arm64, including Linux VMs on Docker
Desktop. The NVIDIA configuration requires Linux, compatible GPUs/drivers, and
NVIDIA Container Toolkit. This is opt-in infrastructure, not an automatically
installed runner service. Install the hooks on **every runner sharing the host**
before leaving it running unattended.

## Workloads

| Mode | Work | Results |
| --- | --- | --- |
| `fuzz` | Rotate the existing H264 fragment and sparse flash extent targets in 120-second campaigns | Logs, Go coverage corpus cache, crash inputs |
| `mutation` | Run Gremlins v0.6.0 on selected packages once per source/configuration | Machine-readable mutations and execution log |
| `review` | Ask Qwen about numbered Go source excerpts, once per source/configuration | Validated JSON hypotheses with evidence and proposed regression tests |
| `race` | Repeat selected packages with `-race -shuffle=on` | Logs including replayable shuffle seeds |

Edit `config.json` to choose packages and budgets. Workloads take turns; a large
review queue cannot starve fuzzing. Jobs have a wall-clock deadline, a log-size
cutoff, and a disk reserve check. Model requests have a separate
`MODEL_TIMEOUT_SECONDS` limit, defaulting to 600 seconds. Tests are run with a bounded Go process count.
A killed job terminates its process group, and the next attempt starts with fresh
source. Only regular files and directories from the archive are extracted; links
are omitted. Use trusted source commits because test code executes in the worker.

Mutation testing measures which code changes your tests detect. A surviving
mutant is a test-gap candidate, not proof of a product bug. Qwen results are always
marked `unverified`; this version does not execute generated reproductions. Each
review sees up to 240 lines from one file, with 40-line overlap. It can miss bugs
that require wider context. Generated Go files and test files are excluded.

## Build and try the CPU worker

From the repository root:

```sh
docker build -t wendy-spare-compute:local scripts/spare-compute
git archive --format=tar --output=/tmp/wendy-spare-source.tar HEAD
cd scripts/spare-compute
cp .env.example .env
# Set SOURCE_ARCHIVE=/tmp/wendy-spare-source.tar in .env.
docker compose run --rm worker --list
docker compose run --rm worker --once
```

`--once` runs one job and writes its outcome to the persistent state volume. A
process exit of zero means the scheduler completed; inspect `result.json` for the
job's actual result. `--list` prints the jobs assigned to this worker. Neither
command starts the NVIDIA services.

The default worker has a 2-CPU ceiling and 4 GiB RAM ceiling, with no swap beyond
that RAM limit. CPU shares of 128 and nice level 19 lower its scheduling priority.
Set `WORKER_CPUS` and `WORKER_MEMORY` to the host's spare capacity; the defaults do
not attempt to consume the entire machine. Memory is used for test/build caches
and, when enabled, model weights and inference. No synthetic memory filler runs.

For multiple machines, give each a unique `WORKER_INDEX` from zero through
`WORKER_COUNT - 1`. Keep the archive, config, and count identical. Job hashes assign
each target/excerpt to one worker. There is no central coordinator or automatic
failover, so work assigned to an offline worker waits. Each worker needs its own
state volume. With many workers, only the shards assigned fuzz/race jobs continue
indefinitely after their finite jobs finish.

## Local Qwen on NVIDIA

Qwen's [model card](https://huggingface.co/Qwen/Qwen3.8-Flash-Next) distinguishes the
open-weight `Qwen3.8-Flash-Next` from the hosted `Qwen3.8-Flash`. This stack serves
the local model through a private Compose network. The worker supports an
OpenAI-compatible `/v1/chat/completions` endpoint, with no API credentials or
public port by default.

The [vLLM recipe](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next) requires v0.30.0
or newer and lists hardware-specific configurations. The full FP8 checkpoint is
about 173 GiB; its host-memory offload also needs substantial RAM. The default
single-GPU example therefore needs a high-memory accelerator. It is **not** a
configuration for an arbitrary 24 GiB runner GPU. Choose a compatible smaller
quantization or a shared local model server when the full weights do not fit.

Create `MODEL_CACHE` on the host, owned by UID/GID 10001. Set the NVIDIA variables
in `.env` after checking the machine's RAM, VRAM and disk capacity:

- `VLLM_IMAGE` selects the serving runtime. Pin a tested image digest for deployment.
- `MODEL_WEIGHTS` selects the checkpoint; the default is the official FP8 release.
- `GPU_ID` selects one GPU. For multiple GPUs, edit `device_ids` in
  `compose.nvidia.yaml` to an explicit list and set `MODEL_TENSOR_PARALLEL` to match.
- `MODEL_MEMORY` is a hard RAM ceiling, defaulting to 128 GiB.
- `MODEL_GPU_MEMORY_FRACTION` defaults to 0.80. This is a server allocation target,
  not a GPU scheduling priority or a guarantee of free VRAM.

```sh
docker compose -f compose.yaml -f compose.nvidia.yaml pull model
docker compose -f compose.yaml -f compose.nvidia.yaml create
docker compose -f compose.yaml -f compose.nvidia.yaml start
docker compose -f compose.yaml -f compose.nvidia.yaml logs -f model
```

The first model startup downloads weights. Do this during maintenance and check
that the server becomes ready. Unavailable endpoints leave the excerpt pending
and retry after other work, with a delay. Invalid/truncated responses create failed
reports. Model startup, inference throughput, and VRAM release must be checked on
the destination NVIDIA host; the local smoke tests do not establish those facts.

A previously configured model endpoint can be used without the NVIDIA override:
set `MODEL_BASE_URL`, `MODEL_NAME`, and `MODES=fuzz,mutation,review,race`. Stopping
this worker does not unload an external model server. Integrate that server with
the host's job hooks too if it shares CI resources.

## Yield to CI

CPU priority cannot free RAM or VRAM. The host hooks stop **all** services before
a runner job begins and restart existing containers only after the last local job
ends. They serialize concurrent hooks with a file lock and retain one busy marker
per runner. Containers have no automatic restart policy, so a Docker restart does
not bypass the hooks. Stopping unloads the model; resuming pays the model load cost.

Copy this directory to a stable installation path such as `/srv/wendy-spare`,
outside any CI workspace that gets cleaned. Prepare its `.env`, source archive,
images, cache, and containers before installing hooks. Use the same Docker daemon,
Compose project, directory, control path, and `--nvidia` setting for all runners.
The runner service account needs Docker access and write access to the control
directory. Host hooks require Python 3.9 or later and Docker Compose v2.

Create two executable shell wrappers outside the checkout:

```sh
# /srv/wendy-spare/job-started.sh
#!/bin/sh
exec python3 /srv/wendy-spare/runner_hook.py started \
  --directory /srv/wendy-spare --control /srv/wendy-spare/control --nvidia
```

```sh
# /srv/wendy-spare/job-completed.sh
#!/bin/sh
exec python3 /srv/wendy-spare/runner_hook.py completed \
  --directory /srv/wendy-spare --control /srv/wendy-spare/control --nvidia
```

Omit `--nvidia` for a CPU-only installation. Set these in each runner's `.env`,
then restart its runner service during maintenance:

```dotenv
ACTIONS_RUNNER_HOOK_JOB_STARTED=/srv/wendy-spare/job-started.sh
ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/srv/wendy-spare/job-completed.sh
```

GitHub documents [job hooks](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/run-scripts).
If hooks already exist, call these wrappers from them and propagate failures.
`RUNNER_NAME` must uniquely identify each runner on the host. Stop errors fail the
start hook and keep a busy marker; allowing CI to proceed would risk contention.
After a crash or a missing completion hook, inspect `control/*.busy`, confirm that
runner has no active job, and remove only its stale marker. Then resume:

```sh
python3 /srv/wendy-spare/runner_hook.py resume \
  --directory /srv/wendy-spare --control /srv/wendy-spare/control --nvidia
```

Do not use `docker compose up` or `start` to bypass busy markers on an active CI
host. Initially activate the worker and install hooks only when all runners are
idle. The hooks track local runner assignments, not arbitrary host processes or
queued GitHub jobs. Use a dedicated runner pool or scheduler integration for
other workloads. Frequent short idle gaps may be unsuitable for loading this model.

## Results, recovery, and source updates

The CPU worker uses the `state` volume. The concurrent NVIDIA reviewer uses
`review-state` with the same layout:

- `reports/<timestamp>-<mode>/result.json`, with source commit, shard, command,
  exit code, duration, and outcome. Logs and workload reports sit beside it.
- `corpus/`, with crash/regression inputs copied out of the disposable workspace.
- `cache/build/fuzz/`, maintained by Go for coverage-increasing inputs.
- `checkpoint.json`, recording completed finite work and the next scheduled job.

Inspect or export without starting the worker:

```sh
docker compose cp worker:/state/reports ./reports
docker compose cp worker:/state/corpus ./corpus
# When using the NVIDIA override, also export reviewer:/state/reports.
```

Report directories rotate at `MAX_REPORTS`, default 200. Export findings before
retention removes them. Crashes remain in `corpus` and may reproduce on later
campaigns. Caches, corpora and downloaded model weights are not deleted
automatically. A worker pauses below `MIN_FREE_GIB`, default 10 GiB. Use a host
filesystem quota for a hard disk limit; the reserve is polled, not reserved, and
the model downloader has a separate cache. Clear build/module caches or increase
the quota while stopped if the worker runs out of space. Keep crash corpora.

Failed/timed-out mutation jobs and invalid review responses remain visible but
advance the queue; they do not count as successful testing. Stop the stack and
remove `checkpoint.json` to repeat a sweep after fixing its cause. Interrupted
jobs retry; a mutation campaign starts again rather than resuming mid-package.
Reduce the selected package scope if campaigns cannot finish within the budget.

For a new revision, stop the stack during maintenance, export a new archive of a
trusted commit, and recreate the workers so their read-only bind mounts see the new
file. The commit/configuration hash resets scheduling while retaining corpora and
caches. The worker never fetches branches or runs untrusted PR submissions.

## Verification

```sh
python3 -m unittest discover -s scripts/spare-compute -p 'test_*.py' -v
docker build -t wendy-spare-compute:local scripts/spare-compute
python3 scripts/spare-compute/smoke_test.py
```

The smoke test uses a temporary Go module and real fuzzing, Gremlins, and race
tests in the restricted image, then stops and resumes a real Compose worker
through overlapping runner hooks. It removes its own temporary volumes. The unit
suite covers process termination, disk pressure, archive boundaries, report
validation, restart checkpoints, retention, and overlapping runner hooks.

Useful next workloads include parser differential tests against an independent
implementation, long container/network lifecycle soak tests on dedicated device
fixtures, and replay of captured failures against new revisions. Prioritize new
fuzz oracles for wire formats and state transitions; extra CPU cannot improve a
fuzzer whose assertions do not detect incorrect behavior.
