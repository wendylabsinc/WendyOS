package mcp

import (
	"math"
	"strings"
)

// compactWorkflowV1Result aggregates only fully known successful outcomes.
// Full per-step evidence was saved before this renderer runs. Unknown, failed
// or inconsistent evidence returns the unmodified detailed result.
func compactWorkflowV1Result(receipt map[string]any) map[string]any {
	if receipt["reported_state"] != "completed" {
		return receipt
	}
	for key, value := range receipt {
		if strings.HasSuffix(key, "_error") && value != nil {
			return receipt
		}
	}
	artifacts, artifactMap := receipt["artifacts"].(map[string]any)
	path, pathKnown := receipt["receipt_path"].(string)
	hash, hashKnown := receipt["receipt_sha256"].(string)
	if !artifactMap || artifacts["complete"] != true || !pathKnown || path == "" || !hashKnown || len(hash) != 64 {
		return receipt
	}
	compact := compactWorkflowResult(receipt)
	obs, ok := receipt["observation"].(map[string]any)
	if !ok || obs["step_check"] != "passed" {
		return receipt
	}
	expected, known := exactWorkflowInt(obs["expected_steps"])
	omitted, complete := exactWorkflowInt(obs["step_outcomes_omitted"])
	steps, typed := obs["step_outcomes"].([]any)
	if !known || expected < 1 || expected > 32 || !complete || omitted != 0 || !typed || len(steps) != expected {
		return receipt
	}
	seen := map[string]bool{}
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			return receipt
		}
		name, named := step["label"].(string)
		code, codeKnown := exactWorkflowInt(step["returncode"])
		timedOut, timeoutKnown := step["timed_out"].(bool)
		if !named || name == "" || seen[name] || !codeKnown || code != 0 || !timeoutKnown || timedOut {
			return receipt
		}
		seen[name] = true
	}
	// Copy decision facts and explicit unknowns; do not mutate the saved receipt.
	out := map[string]any{}
	for key, value := range compact {
		out[key] = value
	}
	observation := map[string]any{"step_check": "passed", "expected_steps": expected, "zero_exit_steps": expected, "timed_out_steps": 0}
	for _, key := range []string{"execution_seconds", "transport_retries", "warnings"} {
		if value, exists := obs[key]; exists {
			observation[key] = value
		}
	}
	out["candidate"] = "Wendy MCP V1"
	if value, exists := receipt["workflow_sha256"]; exists {
		out["workflow_sha256"] = value
	}
	out["observation"] = observation
	return out
}

func exactWorkflowInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= -2147483648 && n <= 2147483647 && n == math.Trunc(n) {
			return int(n), true
		}
	}
	return 0, false
}
