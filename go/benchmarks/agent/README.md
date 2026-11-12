# Agent resource benchmarks

Measure CPU, memory and disk I/O outside the agent, then compare repeated runs.
The collector uses Python 3.10+ and the standard library. Collection requires
Linux; report comparison also works on macOS and Windows.

## Compare two revisions in CI

Run **Agent performance comparison** in GitHub Actions. Choose `baseline_ref`
and `candidate_ref`. It builds both with the same Go toolchain, alternates their
order across three repetitions, and starts each run in a fresh Docker container.
The job summary shows improvements, regressions and repetition ranges. Download
the `agent-performance` artifact for JSON reports, raw samples, logs and HTML
charts. This workflow is manual and does not modify a device or publish a PR comment.

These CI measurements cover an unenrolled agent's idle and local telemetry paths
without containerd or cloud access. They are informational because hosted runners
vary. Agent data uses Docker's writable filesystem; agent stdout and benchmark
artifacts use the host's tmpfs. These are not SD-card/NVMe or production-systemd
baselines. PR CI runs a short real-agent smoke test to validate the tooling.

The same comparison can run locally with Docker and two Linux binaries matching
Docker's architecture:

```sh
python3 go/benchmarks/agent/pair.py \
  --baseline /path/to/baseline/wendy-agent \
  --candidate /path/to/candidate/wendy-agent \
  --baseline-revision BASE_COMMIT --candidate-revision HEAD_COMMIT \
  --output /tmp/agent-comparison
```

The default CI profile takes about nine minutes plus build/container startup time.
Use `--profile go/benchmarks/agent/profiles/smoke.json` only to check the tooling.
Its very short samples are unsuitable for performance conclusions. Files the
container writes are handed back to the owner of the output directory.

## Measure the installed service on hardware

Copy this directory to the device. Run the baseline agent, collect a report,
install the candidate through the usual deployment process, and collect another
report with identical configuration and initial state. The collector does not
install, restart or configure the agent. Commands in a custom profile can do so
explicitly.

```sh
sudo python3 go/benchmarks/agent/benchmark.py run \
  --environment pi5-nvme-unenrolled \
  --revision BASE_COMMIT \
  --metadata /path/to/environment.json \
  --observe-service containerd.service \
  --observe-service systemd-journald.service \
  --output /dev/shm/agent-baseline

# Repeat with the candidate running, using the same profile and metadata.
sudo python3 go/benchmarks/agent/benchmark.py run \
  --environment pi5-nvme-unenrolled \
  --revision HEAD_COMMIT \
  --metadata /path/to/environment.json \
  --observe-service containerd.service \
  --observe-service systemd-journald.service \
  --output /dev/shm/agent-candidate

python3 go/benchmarks/agent/benchmark.py compare \
  /dev/shm/agent-baseline/report.json /dev/shm/agent-candidate/report.json \
  --output /tmp/agent-comparison
```

All output directories must be new. Use a tmpfs or separate storage with enough
space for raw samples. Copy artifacts off tmpfs afterward. The collector and
workload generator must run outside the measured service cgroup. Their CPU and
memory can still influence a small device; check collector overhead before
choosing a faster sample interval. `--pid PID` supports process-only measurement
without systemd and deliberately excludes cgroup totals. Service measurement
needs the cgroup v2 unified hierarchy and stops with an error without it; on
cgroup v1 or hybrid hosts, use `--pid`.

The standard profile takes about 125 minutes. It repeats ten-minute idle and
telemetry workloads five times, with a two-minute settling period before each
workload and thirty seconds afterward for delayed writes. The settling period
does not send workload traffic. Telemetry sends 1,000 records/second, enough to
exercise the default 4 MiB segment rotation and 100 MiB retention limit.

Keep power mode, CPU governor, cooling, filesystem, free disk space, cloud state,
logging settings, agent build flags and application load fixed. Record them in
the metadata JSON, for example:

```json
{
  "storage": "dedicated-nvme-ext4",
  "power_mode": "performance",
  "cloud": "unenrolled",
  "applications": [],
  "go_version": "go1.27.4",
  "build_flags": "CGO_ENABLED=0 -trimpath",
  "initial_state": "same telemetry and image-cache snapshot"
}
```

The tool also records kernel, CPU, RAM, OS release and the running executable's
SHA-256. Metadata must match for comparison; revision and binary digest may
differ. Alternate baseline and candidate captures on the same hardware to reduce
temperature/time bias. Separate cold image-cache, warm image-cache and OS page-cache
experiments. Restoring an image cache does not clear the OS page cache.

## Add deployment, recording, mesh or recovery workloads

Copy a profile and add a `command` scenario. Commands are argument arrays and run
without an implicit shell. Use absolute paths; `cwd` is optional. Each command
has a timeout. A nonzero exit, failed verification, agent exit/PID reuse, counter
reset or observed OOM makes the report invalid for comparison.

```json
{
  "name": "cached-deployment",
  "kind": "command",
  "operations": 1,
  "timeout_seconds": 300,
  "setup": ["/opt/agent-bench/reset-cached-fixture"],
  "ready": ["/opt/agent-bench/check-agent-rpc"],
  "command": ["/opt/agent-bench/deploy-fixed-image"],
  "verify": ["/opt/agent-bench/check-app-output"],
  "cleanup": ["/opt/agent-bench/remove-fixture"]
}
```

These executable paths are examples to implement for the target device.
`setup` runs before every repetition; PID discovery follows setup and readiness.
`command` and required `verify` both contribute to measured duration. `cleanup`
runs even after failure. Child processes left by a command are terminated when
it finishes: SIGTERM, then SIGKILL after two seconds. For a detached deployment, verification must wait for container
readiness and actual app output. Use exit status to reject dropped records or
incomplete work. `operations` must describe the fixed amount of work verified.

The existing [deployment scenarios](../../../scripts/deploy-bench/README.md)
can supply the deploy command through SSH from the device to a build host.
Building on the measured device changes the whole-device workload. Their first
run can populate image caches; choose and prepare that state explicitly. Keep
the CLI version fixed when isolating agent changes.

For cloud disconnect/recovery, restore connectivity in `cleanup`. For recording,
verify exported record counts. Use fixed peers and traffic for mesh workloads.
Custom scenarios are supported on hardware; the isolated Docker pair runner
only accepts idle and telemetry scenarios without hooks. Startup timing and coordinated multi-device
fixtures are not built in.

Telemetry checks every HTTP response and rejects runs that fall behind their
configured offered rate. An HTTP acknowledgement confirms receiver acceptance,
not durability, retention or cloud delivery. An optional `verify` command can
check those outcomes after a telemetry/idle workload, outside its timed phase.
Use a separate scenario if verification cost itself is under investigation.

## Read results and set thresholds

`report.json` contains per-run summaries; `*.jsonl` holds timestamped settle,
measure and cooldown samples. Open `charts.html` for each run's CPU, RSS and
read/write-rate charts. Comparison produces `comparison.md` and
`comparison.json`, using medians and showing min/max across repetitions.

- CPU uses 100% for one occupied core and includes user/system breakdowns.
  Completed workloads also report CPU seconds per operation. Process CPU, like
  process I/O, includes child processes the agent has waited for; a child counts
  when it exits, and `children_cpu_seconds` shows that share.
- Process RSS is separate from service memory, which includes charged file cache
  and kernel memory. Sampled peaks may miss brief spikes. The lifetime RSS
  high-water mark is retained but may predate the measurement window.
- Process `rchar`/`wchar` are logical I/O. `read_bytes`/`write_bytes` are kernel
  storage accounting, with delayed/cancelled-write caveats. Storage totals include
  cooldown; rates and charts describe the measured phase. Byte totals per
  operation use those cooldown-inclusive totals.
- Cgroup I/O and host disks remain separate per device to avoid double counting
  partitions and stacked devices. Host samples include pressure, disk timing,
  temperature and CPU frequency where exposed and readable. Whole-device totals include the
  generator, collector and background services; do not attribute them to the agent.
- Unsupported or inaccessible counters are absent and displayed as unavailable,
  never zero. A change in available metrics invalidates a comparison. Cgroup
  `io.stat` is the exception: the kernel lists a device only after the cgroup's
  first I/O to it, so an unlisted device counts as zero.

The default policy requires both a 10% change and a metric-specific absolute
floor, such as 0.5 CPU percentage points or 5 MiB RSS. These are starting values,
not established Wendy performance budgets. A change smaller than one counter
step, such as one CPU clock tick over the measured phase, is always within
tolerance, so short profiles do not report tick noise. Changes within tolerance are not
proof of equivalence; review repetition ranges and repeat noisy comparisons.

Use `--policy policy.json` to replace the default metrics and thresholds:

```json
{
  "agent.process.rss_bytes.peak": {"relative": 0.05, "absolute": 2097152},
  "agent.process.write_bytes.total": {"relative": 0.10, "absolute": 1048576},
  "workload.operations_per_second": {"relative": 0.05, "absolute": 0.1, "direction": "higher"}
}
```

Add `--fail-on-regression` to comparison or pairing once thresholds are calibrated
on stable hardware. Comparison and pairing exit 0 for an informational report,
1 for a gated regression, and 2 for invalid/incompatible inputs or a failed run.
Pairing checks the policy before it starts Docker. Failed runs retain partial
artifacts and cannot become a baseline. Both missing and newly missing metrics
remain distinguishable in the reports.

## Validate changes to the tooling

```sh
python3 -m unittest discover -s go/benchmarks/agent -p 'test_*.py' -v
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/agent ./go/cmd/wendy-agent
python3 go/benchmarks/agent/smoke.py --agent /tmp/agent --output /tmp/agent-smoke
```

Use `GOARCH=amd64` for x86 Docker. The smoke test checks the real receiver, persisted
fixture data, resource reports, comparison output and rejection of a stopped agent.
