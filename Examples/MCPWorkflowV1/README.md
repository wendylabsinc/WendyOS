# Wendy MCP V1 (experimental)

Start the native server with an explicit, already-authorized target:

```sh
wendy mcp serve --device <exact-selector> --tool-profile workflow-v1
```

The focused catalog contains `wendy_status`, `job_submit`, `job_inspect` and
`job_artifacts_pull`. Use the standard profile for discovery or authentication.
The profile changes discovery/result rendering; it does not grant execution
permission, change the target or add a server-side code interpreter.

Batch dependent calls through the client's existing approved MCP dispatcher.
`client.mjs` accepts that dispatcher as a callback. Never run a raw MCP client
through shell to bypass an assistant's tool approval. `runVerifiedCycle` performs
an explicitly requested completed-job reuse/unchanged-file test, returning only
the decisive second result. It stops on missing evidence or denial. Use this
reuse test only when required by the task; an ordinary workflow needs one call.

Use a long documented client tool yield/wait (up to 60 seconds in the measured
host) around an internally waiting call. Count every outer wait in benchmarks.
`recoverObservation` never submits, reconnects or changes target. It reads only
the same confirmed job path, bounds polling and transient-unavailable retries,
and leaves collection/verification explicitly incomplete afterward. A trusted
client may provide `inspection_target` when its pinned mesh identity differs
from the Cloud selector. Missing or changed identity stops recovery. Configure
the host dispatcher's RPC timeout within the remaining observation budget;
cancelling observation must not cancel the detached job.

Known successful step exits are summarized as `zero_exit_steps`, `expected_steps`
and `timed_out_steps`. Individual outcomes/durations remain in the saved receipt.
Required scalar values, requested digests, warnings and explicit unavailable
fields remain inline. Unknown, failed or inconsistent results retain detailed
evidence. Receipt and manifest paths/hashes remain available separately; neither
a digest nor successful process exits prove scientific/physical success.

The three-arm experimental comparison freezes the previous implementation,
Wendy MCP V1 and a strong reusable stdlib helper. Charge cold configuration,
client startup, schema reads, every model request/wait, retries and failures.
Report total/cached/uncached/output tokens, correctness and orchestration time
separately. The initial cohort uses guarded local CPU fixtures only. It is not
CUDA, Cloud networking, robotics development or fresh-machine installation
evidence. Earlier cohorts and failures remain separate. No 10x claim is assumed.

Run the client contract tests with:

```sh
node --test Examples/MCPWorkflowV1/client.test.mjs
```

See the [source-backed research note](../../specs/2026-10-22-wendy-mcp-v1-research.md)
for which ideas come from Cloudflare, Anthropic, GitHub and Notion, and which are
Wendy design choices.
