---
name: wendy-training
description: Inspect compute capacity, submit authorized durable host training jobs, follow bounded progress, and retrieve verified reports through Wendy CLI or MCP. Use for existing RL and training workflows on Spark or Linux cloud devices.
---

# Wendy Training

Use the exact authorized device selector. Preserve existing jobs, checkpoints,
optimizer/RNG state and unrelated work. A receipt, live PID or idle GPU does not
establish training success or physical robot behavior.

For a focused agent catalog, configure `wendy mcp serve --device <selector>
--tool-profile training`. This changes discovery only; each tool retains its
permissions. For an existing saved workflow, `--tool-profile workflow` advertises
only the workflow-file input for submission. It retains the same authorization.

Experimental **Wendy MCP V1** uses `--tool-profile workflow-v1` with an explicit
startup target. It advertises four workflow tools and concise known-success
counts; full outcomes remain in the saved receipt. Read
[V1 client behavior](references/workflow-v1.md) when using that profile.

For a project-owned seed/condition grid, use
`experiment_plan(recipe_path="/absolute/recipe.json", destination="/absolute/new-workflow.json")`
or `wendy job plan /absolute/recipe.json --output /absolute/new-workflow.json`.
Planning writes a new local file without executing it. Read
[experiment recipes](references/experiment-recipes.md) only when authoring one.
The default catalog remains available for general device work.

1. Call `wendy_status`; connect to the selected target if needed.
2. Use `compute_status` (`wendy device compute-status`) for GPU identity,
   current compute processes and host capacity. Missing tools stay unknown;
   capacity is sampled, not reserved. Do not launch onto a competing workload.
3. For an existing job, use `job_inspect(job_path=...)`. Its directory contains
   `status.json`; direct children can contain `metrics.jsonl`. The probe checks
   OS PID/command/start-time identity separately from recorded state and reads
   the last complete progress record. Use these observations directly; repeat
   raw reads only for missing/contradictory evidence or an independent check.
4. Save its cursor. Use `since` and `wait_seconds` (up to 30) to wait for changed
   progress; unchanged replies are small. Recorded state alone is not liveness.
5. Use `job_artifacts_pull(job_path=..., files=[...], destination=...)` for
   selected relative reports. It returns a verified local manifest and skips
   files whose hashes match. Bytes stay out of agent context. Maximum 64 files,
   16 MiB transferred; request fewer files when needed. Destination is absolute.

Before a checkpoint restart, use `checkpoint_inspect(checkpoint_path=...,
sha256=..., python=..., source_directory=...)` to inspect the trusted receipt's
artifact through the existing remote Torch/NumPy interpreter. It returns counters,
saved-state availability, source differences and matching environment-adapter
files. Empty saved experiment RNG state is distinct from a missing field. Preserve
the original adapter and stress profile as well as policy, optimizer and RNG.
Source checks and readable metadata are evidence for planning, not proof that a
restart is compatible or successful. The inspector uses restricted CPU loading,
starts no training and installs nothing. It retains host-execution authorization
because an explicit interpreter runs with host-agent permissions. Files above
64 MiB or unsupported serialization remain unknown; it never falls back to an
unrestricted pickle loader. The workflow catalog stays focused; the training
catalog includes checkpoint inspection.

CLI: `wendy job checkpoint inspect /remote/iteration.pt --sha256 <receipt-digest>
--python /remote/.venv/bin/python --source-directory /remote/project`.

For a new job explicitly requested by the user, prefer a saved local workflow:
`job_submit(workflow_path="/absolute/local/workflow.json")`. Supply it alone.
The versioned file combines the job and its evidence contract:

```json
{
  "schema": "wendy.workflow.v1",
  "job": {
    "name": "evaluate-001",
    "working_directory": "/remote/project",
    "steps": [{"name": "evaluate", "argv": [".venv/bin/python", "evaluate.py", "--output", "{step_dir}/result.json"], "timeout_seconds": 600}]
  },
  "wait_seconds": 660,
  "artifact_files": ["status.json", "evaluate/result.json"],
  "artifact_destination": "/local/reports/evaluate-001",
  "report_fields": {"evaluate/result.json": ["/successes", "/episodes"]}
}
```

The workflow/spec file is local to the MCP server; its working directory and
argv refer to the connected remote host. The file cannot override the target.
Use unique output directories for separate workflows. For ad hoc submission,
`spec_path="/absolute/local/spec.json"` or inline `spec=...` remains available,
with optional wait/artifact/report arguments. Prefer references over copying
JSON through model context. Step names must be unique; `runner` is reserved
for the job supervisor and is rejected before submission.

Waiting is bounded to 3600 seconds through host RPCs of at most 30 seconds.
Set the client's tool timeout above the wait plus submission/collection time
(for example, wait + 300 seconds). Expiry never cancels the job; inspect it or
retry the identical workflow to collect later. Observation errors preserve the
confirmed receipt and leave completion unknown.
Terminal waiting retries a transient unavailable observation stream at most
three times, on the same authenticated connection and exact job path. It never
resubmits a job, changes targets, or retries permission/missing-evidence errors.
Recovered observations include `transport_retries`; exhausted retries preserve
the original receipt and an explicit observation error.

Keep the assistant's outer wait out of repeated model decisions as well. When
using a code-mode tool wrapper, use its documented long yield (up to 60 seconds)
and the same bounded wait for a still-running call. Avoid 1-second polling of a
workflow that already waits internally. Use a known tool directly once its
schema is available; do not rediscover it before each invocation. Count wrapper
calls and waits when measuring agent tokens, not only nested MCP calls.

After terminal observation the response includes every declared step's exit
and timeout outcome, plus `step_check`. New jobs record monotonic per-step wall
durations, total execution time and the runner source hash. These durations
include process startup, runtime and I/O; they are not GPU-only measurements.
Keep framework training/evaluation loop time separate when comparing agent
orchestration. Older jobs can have unknown timings. A passed step check verifies process
exits, not scientific success. The top-level state reflects the latest
observation. Failed workflows include one bounded log tail (at most 2048 bytes)
and its path/hash so the agent can diagnose ordinary failures without another
log fetch; unavailable logs stay explicit. Selected reports are retrieved with digest
verification; their bytes stay outside context. `report_fields` returns at most
32 scalar JSON-pointer selections across eight files, with report hashes.
Missing, compound, oversized or changed fields are explicitly unavailable;
never infer zero or success from them. Full reports remain on disk for audit.

After terminal observation, a compact receipt is saved even if selected report
collection fails; collection errors remain explicit. The receipt is saved as `.wendy-artifacts-workflow-receipt.json` in the
artifact directory. The response links its path and SHA-256, the manifest and
workflow hashes. A receipt proves provenance, not independent outcome grading.
A running job returns `artifacts_pending`; an `artifact_error`, `report_error`
or `receipt_error` preserves the job identity. An identical retry reuses that
job and skips unchanged reports. Use `job_artifacts_pull` for larger selections
or ongoing jobs, subject to the same 64-file/16-MiB transfer limits.

The spec has `name`, absolute `working_directory`, and 1–32 `steps`. Each step
has a unique `name`, `argv` and `timeout_seconds` (1–86400). Optional
`source_hashes` verify relative source/checkpoint files before a new launch.
For large inventories, use `source_manifest: {"path": "sources.json", "sha256":
"<digest>"}`. This references a remote JSON contract (at most 1 MiB):
`{"schema":"wendy.sources.v1","files":{"project/train.py":"<sha256>"},
"exact_directories":["project/rl"]}`. It verifies up to 4096 files without copying
hash inventories into tool arguments. Each exact directory must contain exactly
the declared direct files; subdirectories such as bytecode caches are ignored.
Checks reject symlinks and run before allocating a new job. A retry retrieves
the existing job and its original source-check evidence; it does not revalidate
current files or launch again. Use a new name for a new source validation/run.
A step can declare `metrics_file: "output/metrics.jsonl"`, relative to its own
step directory, to expose nested trainer progress through `job_inspect` without
extra raw reads. Use the
existing project interpreter/venv; no implicit installation is performed.

Job metadata and logs are stored under `<working_directory>/.wendy/jobs/<name>`.
Commands run with host-agent permissions and may write elsewhere: direct outputs
to `{job_dir}` / `{step_dir}`, or use `WENDY_JOB_DIR` / `WENDY_STEP_DIR`.
Steps must run in the foreground; daemonizing workloads belong in a managed app.
Identical name/spec retries return the original job; use a new name to rerun.
Inspect the returned path and verify outcomes. Submission is not completion.
Never submit physical robot actions without specific user authorization.

CLI equivalents: `wendy job inspect <path> --since <cursor> --wait 30`,
`wendy job artifacts pull <path> --file status.json --output <local-dir>`,
and `wendy job submit workflow.json --workflow`. Use the global `--device` selector.
Add `--artifact status.json --artifact evaluate/result.json --output /local/reports`
to a plain spec submission with `--wait 660` to collect reports after terminal
evidence. Add `--report-field evaluate/result.json#/successes` for a scalar
projection. A saved workflow owns these options; do not also pass overrides.

The `workflow` profile keeps successful replies compact: step exits/durations,
selected values and explicit unknowns remain inline; process/spec/runner/report
hashes remain in the digest-bound local receipt; the artifact manifest reference
also remains inline so a needed detail does not require a receipt lookup. Other
profiles and incomplete/error replies retain detailed evidence inline. Read the
saved receipt when a provenance detail is needed instead of rerunning the job.
Progress does not warn about metrics for steps that have not started. While a
step runs, `recorded_execution_seconds` is the last saved worker timing, not a
live elapsed timer; terminal observations expose `execution_seconds`.

When the task explicitly needs selected file SHA-256 values, add
`"artifact_digests": ["status.json", "evaluate/result.json"]` to the saved
workflow. Select only collected files, at most16 unique names and4096 encoded
reply bytes. These hashes are rechecked against local bytes and the verified
manifest, returned inline and saved in the receipt. This avoids loading the
whole manifest into context just to obtain a few hashes. Omit it otherwise.
The full submission schema also accepts `artifact_digests`; CLI specs can use
repeated `--artifact-digest <collected-relative-file>` flags.
