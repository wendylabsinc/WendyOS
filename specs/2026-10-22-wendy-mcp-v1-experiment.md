# Wendy MCP V1: frozen CPU comparison

Wendy MCP V1 did not establish an overall performance improvement in this
exploratory cohort. Across the two equally represented tasks, V1 used 1.1% more
total tokens and 42.9% more orchestration time than the previous implementation.
Against the strong regular helper, it used 10.4% fewer total tokens but 54.3%
more orchestration time. All 18 attempts eventually passed the shared grader.
No dollar-cost or 10x claim is supported.

## Candidate and comparison

- **Previous:** pinned commit `b9e04df76`, nine-tool workflow profile. Already
  supports saved workflows, client batching guidance, terminal waits, bounded
  observation retries and verified selected reports with full receipts.
- **Wendy MCP V1:** four-tool opt-in `workflow-v1` profile, known-success exit
  counts, preserved required scalars/digests/unknowns and full evidence separately.
  Client examples batch approved calls and recover only confirmed observations.
  Batching, local answer verification and long outer waits are encouraged for
  both MCP arms. V1 is a code-plus-guidance bundle, not a code-only ablation.
- **Regular helper:** reusable stdlib execution, identical completed-job reuse,
  checksum-verified collection and unchanged-file skipping. The existing helper
  received timing instrumentation only. It has no Wendy dependency in this CPU
  test; the historical CUDA baseline used Wendy CLI for Cloud transport instead.

This follows the distinctions in the [four-source research note](2026-10-22-wendy-mcp-v1-research.md).
The profile changes discovery/rendering, never execution authority. No arbitrary
server-side code interpreter or new tool permission was introduced.

## Design and accounting

Three interleaved blocks, three arms and two tasks: 18 attempts. Each executes
the same fixed three-phase quadratic CPU workload, 1,000 optimization iterations
and eight deterministic evaluation checks. Both tasks require actual exits,
identical-job reuse, three selected verified artifacts and unchanged retrieval.

The prepared task supplies `workflow.json`. The cold-configuration task requires
the agent to create it from the same spec and evidence requirements. Every arm
gets equivalent source/spec/helper files; the regular arm also creates the
workflow contract. All 18 normalized workflows and non-MCP client settings were
independently checked equal. Frozen binary/driver/grader/source hashes remained
unchanged. Model: `gpt-6-astra`, medium effort, fresh isolated client per attempt.

Timers start before private workspace/config preparation. Client/MCP startup,
configuration authoring, discovery, local verification, model requests, retries
and outer waits remain charged. Orchestration is setup plus agent wall minus
the sum of serial step process durations, which include startup/I/O. Independent
grader wall is also retained separately and in total-through-grading time.

This is cold **workflow configuration/client startup**, not fresh-machine
installation/sign-in, full RL source development, CUDA, Cloud networking or
physical robotics. Installed tools, model authentication and a supplied reusable
helper are shared assumptions. One-time research/build/development costs are
unmeasured and recorded separately; they are not silently zero-cost installation.

## Results

Means per attempt; three attempts per row. Intermediate failed commands remain
included in tokens, requests and elapsed time.

| Task | Arm | Total tokens | Uncached input + output | Model requests | Orchestration seconds | Correct |
|---|---|---:|---:|---:|---:|---:|
| Prepared | Regular helper | 65,295 | 7,012 | 5.00 | 35.62 | 3/3 |
| Prepared | Previous | 48,654 | 5,774 | 3.67 | 40.08 | 3/3 |
| Prepared | Wendy MCP V1 | 39,373 | 5,154 | 3.00 | 41.12 | 3/3 |
| Cold configuration | Regular helper | 52,825 | 10,073 | 4.00 | 36.95 | 3/3 |
| Cold configuration | Previous | 56,008 | 8,520 | 4.00 | 38.27 | 3/3 |
| Cold configuration | Wendy MCP V1 | 66,475 | 7,723 | 4.67 | 70.83 | 3/3 |

For sensitivity to outliers, median token counts were prepared:
78,966 / 53,310 / 39,524 and cold: 52,976 / 56,042 / 55,912, in
regular/previous/V1 order. Median orchestration seconds were prepared:
41.46 / 44.78 / 45.71 and cold: 36.81 / 36.32 / 64.90. Means, medians,
individual attempts and paired directions are all retained; no statistic is
selected to conceal a regression. Cached input/output are recorded separately;
uncached input plus output is a token count, not a dollar bill.

All attempts together consumed 985,890 total tokens over 73 model requests and
55 model-visible API calls. There were zero outer waits in this short CPU
cohort; the accounting still explicitly counts them. Three intermediate local
command failures were repaired: two in the previous arm and one in V1. No native
MCP call failed or required a new permission. No failed attempt was dropped or
rerun to replace its result.

## Interpretation and limits

The native protocol catalog shrank 5,941 to 3,086 serialized bytes (9 to 4 tools),
and its sample result shrank 1,745 to 1,570 bytes. These byte reductions are not
full-task token savings. Initial model inputs still cost about 12,000 tokens.
The shared server instruction preamble remains generic; a future focused
revision should address that separately and be measured with a new protocol.

V1 used fewer prepared-task model requests, but did not improve mean elapsed
time. During one cold V1 attempt the agent looked for `exit_code` rather than
the saved status file's `returncode`. The smaller summary did not show that
field name; repair added two model requests. That cost remains included. This
is evidence that preserving decisive fields/schema cues matters, not proof that
all observed variation is caused by aggregation.

The callback client passes five contract tests for batching, lost/denied
submission, known-identity observation recovery, target changes and retry limits.
A separate actual native fixture recovery protocol injected one unavailable
observation, recovered with one `job_inspect`, preserved status bytes and made
zero submission calls. Collection remained explicitly incomplete. This verifies
behavior, not fault-recovery performance in the agent cohort.

N=3 per arm/task is exploratory. The workstation and inference service are
shared; code tests ran during early attempts, and inference latency varies.
There is no isolated-machine, statistical-significance, RL/G1 or universal
speed claim. Fresh installation/authentication costs and live CUDA comparison
remain unmeasured. Existing CPU-fixture exceptions stayed restricted to
`job_submit`/`job_artifacts_pull`; normal profiles, the completed one-pilot CUDA
exception and real workloads were unchanged. No new GPU jobs or real device
connections were made.

Private protocol hashes, exact prompts/calls/replies, every usage record,
individual results, failure excerpts and an independent 18-attempt regrade are
retained outside the repository. Earlier cohorts are preserved and not pooled
into these newly matched results.
