package mcp

import "strings"

// compactWorkflowResult keeps decision facts in a successful saved-workflow
// reply. The digest-bound receipt already holds the full process observation,
// source/runner/spec/report hashes and artifact manifest references. Other
// profiles and incomplete/error replies retain those details inline.
func compactWorkflowResult(receipt map[string]any) map[string]any {
	for key, value := range receipt {
		if strings.HasSuffix(key, "_error") && value != nil {
			return receipt
		}
	}
	observation, ok := receipt["observation"].(map[string]any)
	if !ok || receipt["reported_state"] != "completed" || observation["step_check"] != "passed" {
		return receipt
	}
	artifacts, ok := receipt["artifacts"].(map[string]any)
	if !ok || artifacts["complete"] != true || receipt["artifact_error"] != nil {
		return receipt
	}
	path, pathOK := receipt["receipt_path"].(string)
	hash, hashOK := receipt["receipt_sha256"].(string)
	if !pathOK || path == "" || !hashOK || len(hash) != 64 {
		return receipt
	}
	out := map[string]any{}
	for _, key := range []string{"reported_state", "job_path", "target", "reused", "receipt_path", "receipt_sha256", "source_check", "artifact_digests"} {
		if value, exists := receipt[key]; exists {
			out[key] = value
		}
	}
	obs := map[string]any{}
	for _, key := range []string{"step_check", "step_outcomes", "expected_steps", "step_outcomes_omitted", "execution_seconds", "transport_retries", "warnings"} {
		if value, exists := observation[key]; exists {
			obs[key] = value
		}
	}
	out["observation"] = obs
	selected := map[string]any{}
	for _, key := range []string{"complete", "verified_files", "unchanged_files", "manifest_path", "manifest_sha256"} {
		if value, exists := artifacts[key]; exists {
			selected[key] = value
		}
	}
	out["artifacts"] = selected
	if reports, ok := receipt["reports"].(map[string]any); ok {
		projected := map[string]any{}
		for name, raw := range reports {
			report, ok := raw.(map[string]any)
			if !ok {
				return receipt
			}
			// Preserve every explicit unknown; only defer redundant provenance.
			projected[name] = map[string]any{"fields": report["fields"], "unavailable": report["unavailable"]}
		}
		out["reports"] = projected
	}
	return out
}
