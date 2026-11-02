# Wendy MCP V1 (experimental)

Use `wendy mcp serve --device <authorized-selector> --tool-profile workflow-v1`.
The catalog contains `wendy_status`, `job_submit`, `job_inspect` and
`job_artifacts_pull`. Discovery/authentication belongs in the standard profile.
An existing MCP process does not change when new skill text is installed.
Profiles and batching never grant tool execution or permission to change targets.

Submit a saved workflow by file reference. Batch dependent calls through the
client's approved dispatcher and await the same long-running call using the
client's documented long yield/wait. Avoid one-second outer polling. Do not
print intermediate full replies when the final verified result answers the task.
Reuse testing requires a second identical call only when explicitly requested;
ordinary execution needs one. Count every outer wait and discovery request.

Known successful outcomes summarize `zero_exit_steps`, `expected_steps` and
`timed_out_steps`; all individual outcomes/durations remain in the digest-bound
receipt. Required report values, requested file digests, explicit unavailable
fields and warnings stay inline. The workflow hash and receipt/manifest
references remain available. Unknown, failed or inconsistent evidence is
returned in detail. Successful processes and valid hashes do not establish
scientific improvement, resource reservation or physical behavior.

After observation loss, preserve the confirmed target, job path and spec hash.
Retry only bounded read-only observation of that identity; do not resubmit as
recovery or infer that a job failed. Lost submission responses without confirmed
identity, permission errors and changed targets stop. Completed observation
still requires artifact collection, digest checks and independent outcome
verification. The client host must preserve approvals and bound RPC deadlines.
Never bypass assistant approval by invoking a raw MCP client from shell.

Cold comparisons must include configuration, startup, source/workflow
preparation, discovery and connection work actually required. Preserve failures
and earlier cohorts; report tokens, all model requests/waits, orchestration time
and independent correctness separately. Do not treat smaller schemas/replies
as measured task savings or assume 10x. Local CPU fixture evidence does not
establish RL/CUDA, Cloud or physical-device performance.
