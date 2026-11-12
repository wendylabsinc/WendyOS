# Spare compute on Wendy Cloud devices

> **Deployment blocked:** the LAN check on Joannis Thor General found that the
> Agent intentionally does not expose HostShell on its local admin socket. The
> current supervisor receives `UNIMPLEMENTED` and denies all work. This draft
> needs a supported read-only host observation API before the command below
> produces useful background work. Do not expose HostShell locally to work
> around the restriction. Container tests do not verify Agent service availability.

From this directory, deploy the app to a device returned by `wendy discover`:

```sh
wendy run --device <device-selector> --detach --yes
```

Wendy builds and deploys four services: a host-activity supervisor, a Go testing
worker, a Qwen reviewer, and a local NVIDIA model server. There is no GitHub
Actions workflow, runner hook, Docker daemon requirement on the device, or manual
source-archive step. For CPU-only work, deploy `--service worker`; Wendy includes
its supervisor dependency and skips the model images.

The worker fetches the public WendyOS repository's `main` revision after receiving
permission to work. Each attempt records the exact commit. Work sessions recycle after six hours
to fetch a new revision; completed review sweeps check again after an hour. Change
`SOURCE_REPOSITORY` and `SOURCE_REF` in `wendy.json` to select another trusted
repository or pin a commit. No GitHub token is needed. Only run trusted code;
tests execute inside the worker container.

## How spare capacity is measured

The supervisor reads host CPU, memory, cgroup and [NVIDIA process information](https://docs.nvidia.com/deploy/nvidia-smi/index.html)
through the Wendy Agent's local socket. It never executes the test workload on
the host. The fixed probe needs Linux cgroup v2 and host Python 3. Missing CPU,
memory, app-cgroup or host-probe information denies permission to work.

The defaults wait for 60 seconds of idle observations before admitting work:

- Host CPU consumption outside this app must stay below 0.25 CPU cores.
  The probe subtracts all this app's service cgroup CPU usage, so fuzzing and
  inference do not make the app mistake itself for competing work.
- CPU work needs 12 GiB of available host RAM to start and yields below a
  4 GiB reserve. Per-service cgroup limits independently cap RAM and CPU.
- GPU work also requires the selected GPU to be idle and have enough free
  memory for the configured checkpoint. A foreign compute, graphics or other GPU process revokes all
  work, even when that process currently reports zero utilization.
- Display-active GPUs and MIG configurations are excluded. Losing NVIDIA
  visibility on a host with an NVIDIA driver revokes work; a CPU-only host can
  still run CPU tests.

The supervisor writes permissions valid for eight seconds. Each workload monitor
checks every 250 ms. Withdrawal or expiry sends SIGTERM to all workload-UID
processes in that service's private PID namespace, then SIGKILL after four
seconds. This kills inference and releases its allocations instead of merely
pausing the model. The monitors wait at least 60 seconds before trying again.
Supervisor failure, a disconnected host probe, old permissions after reboot,
and malformed permission files all stop work automatically.

This is **reactive preemption**, not a reservation or admission lock. Another
application can start between samples and briefly overlap with this one.
A probe normally samples every two seconds plus its bounded NVIDIA queries;
permissions expire if observations stop. Idle applications that are not using
CPU/GPU and have not put memory under pressure do not block work. Strict zero
interference requires scheduling cooperation from every foreground workload.
The initial `wendy run` build and image transfer happen before this governor can
control its own workloads, so deploy during a suitable maintenance window.

## Resource and model configuration

Edit `wendy.json` before deployment:

| Setting | Default | Purpose |
| --- | --- | --- |
| Worker `resources` | 2 CPUs, 4 GiB RAM | Hard test-container ceiling, no extra swap |
| Reviewer `resources` | 1 CPU, 1 GiB RAM | Bound source review overhead |
| Model `resources` | 4 CPUs, 128 GiB RAM | Hard serving-container ceiling |
| `IDLE_SECONDS` | 60 | Continuous idle period before admission |
| `MAX_FOREIGN_CORES` | 0.25 | CPU usage outside this app that triggers a yield |
| `START_FREE_GIB` / `RESERVE_GIB` | 12 / 4 | Host RAM thresholds for CPU work |
| `MODEL_START_FREE_GIB` | 140 | Available host RAM required before loading the model |
| `MODEL_START_FREE_MIB` | 180000 | Free GPU memory required before loading the model |
| `GPU_INDEX` / `CUDA_VISIBLE_DEVICES` | 0 / 0 | Must identify the same physical GPU |

The supplied model command serves `Qwen/Qwen3.8-Flash-Next-FP8` using vLLM
v0.30.0. Qwen's [model card](https://huggingface.co/Qwen/Qwen3.8-Flash-Next)
distinguishes the open-weight Flash-Next model from hosted Qwen3.8-Flash. The
[vLLM recipe](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next) lists the hardware
requirements and supported quantizations.

The full FP8 checkpoint is about 173 GiB and needs a high-memory accelerator.
The conservative default **will leave GPU work idle on smaller devices**. For
those devices, select a supported quantized checkpoint and adjust the model
command, RAM ceiling and admission thresholds together. Devices with unavailable
GPU memory reporting, including some unified-memory configurations, fail closed;
they need a verified memory accounting strategy before enabling inference.
No model is silently substituted. Model image/driver compatibility, first model
load and VRAM release must be validated on the target NVIDIA hardware.

The model binds inside its bridge network, not the host network. The reviewer
uses `http://model:8000/v1`. First startup downloads weights into the
persistent model cache. Source downloads, test compilation and model loading
all happen under expiring permission. Changing these defaults does not create
extra physical RAM or VRAM.

## Permissions and storage

Only the supervisor has the sensitive `admin` entitlement. It grants full local
Agent authority, although this supervisor uses only a fixed read-only host
probe. Keep its image and source trusted. Test, review and model services have
no admin socket. Their monitors run as root, but launch workloads with UID/GID
10001, an empty supplementary-group list, and no admin socket environment.
Only the root supervisor writes permission files; workload users cannot renew
them. The monitors require private PID namespaces and must not use host PID mode.

Wendy persistent volumes store source snapshots, reports, corpus inputs, build
caches and model weights. Temporary Go executables and mutation work directories
live in `/work` on the container filesystem, because Wendy persistence mounts
are `noexec`. Inference JIT caches also use `/work`; only model data stays
in the persistent model volume. Stopping a process discards its RAM while preserving reports and
corpora. The next attempt extracts pristine source.

The testing and review state volumes contain `reports/<timestamp>-<mode>/` with
`result.json`, `output.log`, and any mutation/review/crash artifacts. Results
include the source commit, command, duration, exit code and shard. `corpus/`
preserves crash inputs; Go's `cache/build/fuzz/` preserves interesting inputs.
Reports rotate at 200 directories. Export findings before retention removes them.
Caches, corpora and model weights are not automatically deleted. The worker
pauses below its 10 GiB disk reserve; use filesystem quotas for a hard bound.
The model cache needs its own disk budget.

Inspect lifecycle and admission decisions through Wendy:

```sh
wendy --json device apps list --device <device-selector>
wendy --json device logs --app sh.wendy.spare-compute --tail 50 --no-follow --device <device-selector>
wendy device apps stop sh.wendy.spare-compute --device <device-selector>
```

## Workloads and distribution

`config.json` selects packages and budgets. CPU jobs alternate between 120-second
fuzz campaigns, Gremlins mutation testing, and shuffled race tests. Mutation
sweeps run once per source/configuration; interrupted jobs retry from pristine
source. Failed or timed-out jobs are retained as failures and advance the queue.
They do not count as successful tests. Remove a stopped worker's
`checkpoint.json` to repeat a finite sweep after fixing its cause.

The reviewer checks overlapping numbered Go excerpts independently of CPU tests.
Results contain evidence and proposed regression tests, and are always marked
`unverified`. This version neither executes model-generated commands nor applies
patches or opens issues. Cross-file bugs may require more context than an excerpt.
A surviving mutation similarly identifies a possible test gap, not a proven bug.

For multiple devices, set a different `WORKER_INDEX` from zero through
`WORKER_COUNT - 1` in each deployment. Keep the source revision, job configuration
and count identical. Hashes assign each target/excerpt to one device; there is no
central queue or automatic failover. Leave the defaults for independent full
sweeps on each device. Corpus exploration remains useful across independent runs.

## Local checks

```sh
wendy project validate scripts/spare-compute
python3 -m unittest discover -s scripts/spare-compute -p 'test_*.py' -v
docker build -t wendy-spare-compute:local scripts/spare-compute
python3 scripts/spare-compute/smoke_test.py
```

Docker is only used here as a local test environment. Production deployment uses
Wendy. Tests cover admission hysteresis, foreign and own CPU/GPU work, memory
pressure, stale permissions, worker interruption, report retention and model
responses. The smoke test runs real fuzzing, race tests and mutations with both
killed and surviving mutants.

Further workloads worth adding are differential parser tests, replay of captured
failures against new commits, and long network/container lifecycle soak tests on
dedicated fixtures. New fuzz oracles for protocol and state-machine invariants
will often find more than simply extending the same campaigns.
