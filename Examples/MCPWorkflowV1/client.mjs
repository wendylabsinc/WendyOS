// Client-side orchestration. The host supplies approved MCP dispatch; this is
// not a server execute/eval tool and must not bypass the host's permission gate.
export function decode(result) {
  if (result.isError) throw new Error("MCP call failed; do not change execution route");
  if (!Array.isArray(result.content) || result.content.length !== 1 || result.content[0].type !== "text") throw new Error("Expected one JSON result");
  return JSON.parse(result.content[0].text);
}

// Batch an explicit completed-job reuse check. This second submission is a
// requested idempotency test, never recovery from a lost submission response.
export async function runVerifiedCycle(call, workflowPath) {
  const first = decode(await call("job_submit", {workflow_path: workflowPath}));
  requireComplete(first);
  const second = decode(await call("job_submit", {workflow_path: workflowPath}));
  requireComplete(second);
  if (!second.reused || second.job_path !== first.job_path || second.target !== first.target || second.workflow_sha256 !== first.workflow_sha256) throw new Error("Job identity changed");
  if (second.artifacts.unchanged_files !== second.artifacts.verified_files) throw new Error("Unchanged retrieval not verified");
  return second; // Full evidence remains in the server's saved, hashed receipt.
}

export function requireComplete(value) {
  const o = value.observation;
  if (Object.keys(value).some(k => k.endsWith("_error") && value[k] != null) || value.reported_state !== "completed" || !o || o.step_check !== "passed" || value.artifacts?.complete !== true || !/^[a-f0-9]{64}$/.test(value.receipt_sha256 || "") || !/^[a-f0-9]{64}$/.test(value.workflow_sha256 || "") || !value.receipt_path || !value.target || !value.job_path?.startsWith("/")) throw new Error("Incomplete evidence; original job stays untouched");
  if (!Number.isInteger(value.artifacts.verified_files) || value.artifacts.verified_files < 1 || value.artifacts.verified_files > 64) throw new Error("Unknown artifact verification count");
  if (!Number.isInteger(o.expected_steps) || o.expected_steps < 1 || o.expected_steps > 32) throw new Error("Unknown step count");
  if (o.step_outcomes) {
    if (o.step_outcomes_omitted !== 0 || o.step_outcomes.length !== o.expected_steps || o.step_outcomes.some(s => s.returncode !== 0 || s.timed_out !== false)) throw new Error("Unknown or failed step");
  } else if (o.zero_exit_steps !== o.expected_steps || o.timed_out_steps !== 0) throw new Error("Unknown or failed step summary");
}

// Recover only observations of an already-confirmed job. Missing identity,
// authorization errors, malformed results and target changes stop immediately.
// Recovered observation is not a completed workflow: collection and hash/report
// verification must still finish separately before requireComplete can pass.
export async function recoverObservation(call, receipt, {budgetMs = 60000, now = Date.now, sleep = ms => new Promise(r => setTimeout(r, ms))} = {}) {
  if (!receipt.job_path?.startsWith("/") || !receipt.target || !/^[a-f0-9]{64}$/.test(receipt.spec_sha256 || "") || !Number.isFinite(budgetMs) || budgetMs <= 0 || budgetMs > 3600000) throw new Error("Confirmed job identity and bounded budget required");
  const deadline = now() + budgetMs;
  const jobPath = receipt.job_path;
  const target = receipt.target;
  const inspectionTarget = receipt.inspection_target || receipt.observation?.target || target;
  let retries = 0;
  let since;
  while (now() < deadline) {
    let observation;
    try {
      const args = {job_path: jobPath, wait_seconds: since ? Math.min(30, Math.max(0, Math.floor((deadline - now()) / 1000))) : 0};
      if (since) args.since = since;
      observation = decode(await call("job_inspect", args));
    } catch (error) {
      if (error.code !== "UNAVAILABLE" || retries >= 3) throw error;
      const delay = 250 * 2 ** retries++;
      if (now() + delay >= deadline) break;
      await sleep(delay);
      continue;
    }
    // A known mesh identity can differ from the Cloud selector. It must come
    // from the client's pinned connection state, never a new discovery choice.
    if (observation.target !== inspectionTarget) throw new Error("Inspection target changed or unavailable");
    if (observation.cursor) since = observation.cursor;
    if (["completed", "failed", "stopped", "cancelled"].includes(observation.reported_state)) return {job_path: jobPath, target, observation, transport_retries: retries, collection_required: true};
    // Some transports return unchanged snapshots immediately. Avoid a busy loop.
    await sleep(Math.min(250, Math.max(0, deadline - now())));
  }
  return {job_path: jobPath, target, completion: "unknown", collection_required: true, transport_retries: retries};
}
