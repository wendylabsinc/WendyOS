package chat

import (
	"fmt"
	"strings"
)

func isActivityEntry(entry chatEntry) bool {
	switch entry.kind {
	case "tool", "result", "agent_start", "agent_done", "agent_memory":
		return true
	}
	return false
}

// Collapse only the presentation, keeping every event available through /tools.
// Consecutive activity shares a row even when child events are interleaved.
func compactActivity(entries []chatEntry, active bool) (string, []string) {
	tools, results := 0, 0
	agents := make(map[string]string)
	var latest string
	var warnings []string
	for _, entry := range entries {
		switch entry.kind {
		case "tool":
			tools++
			latest = strings.TrimPrefix(compactToolEntry(entry), "› ")
		case "result":
			results++
			summary := compactToolEntry(entry)
			name := strings.ReplaceAll(strings.TrimPrefix(entry.title, "Result · "), "_", " ")
			latest = name + " · " + strings.TrimPrefix(summary, "↳ ")
			if activityNeedsAttention(summary) {
				warnings = append(warnings, "! "+name+" · "+strings.TrimPrefix(strings.TrimPrefix(summary, "↳ "), "! "))
			}
		case "agent_start":
			agents[entry.title] = "running"
		case "agent_done":
			state, _, _ := strings.Cut(entry.text, ":")
			agents[entry.title] = state
			if state != "completed" {
				warnings = append(warnings, "! "+entry.title+" · "+toolSummaryLine(entry.text))
			}
		}
	}
	var parts []string
	if tools > 0 || results > 0 {
		count := max(tools, results)
		label := "tools"
		if count == 1 {
			label = "tool"
		}
		parts = append(parts, fmt.Sprintf("%d %s", count, label))
	}
	if len(agents) > 0 {
		finished := 0
		for _, state := range agents {
			if state != "running" {
				finished++
			}
		}
		label := "agents"
		if len(agents) == 1 {
			label = "agent"
		}
		parts = append(parts, fmt.Sprintf("%d/%d %s finished", finished, len(agents), label))
	}
	if active && tools > results {
		parts = append(parts, fmt.Sprintf("%d running", tools-results))
	} else if tools > results {
		parts = append(parts, fmt.Sprintf("%d unfinished", tools-results))
	}
	if latest != "" {
		parts = append(parts, latest)
	}
	if len(parts) == 0 {
		parts = append(parts, "Agent activity")
	}
	return "▸ " + strings.Join(parts, " · "), warnings
}

func activityNeedsAttention(summary string) bool {
	for _, prefix := range []string{"! ", "↳ Tool error:", "↳ Tool reported a failure", "↳ Tool reported an error", "↳ Denied", "↳ Tool was not executed"} {
		if strings.HasPrefix(summary, prefix) {
			return true
		}
	}
	// Background process results include failed jobs in their compact summary.
	return strings.Contains(summary, " · failed · ") || strings.Contains(summary, " failed")
}
