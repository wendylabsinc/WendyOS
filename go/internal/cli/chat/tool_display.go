package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Compact presentation never changes the complete tool results in the engine
// or the arguments displayed for approval. /tools reveals the stored details.
func compactToolEntry(entry chatEntry) string {
	if entry.kind == "tool" {
		name := strings.TrimPrefix(entry.title, "Tool · ")
		label := chatSingleLine(strings.ReplaceAll(name, "_", " "))
		var args map[string]any
		if json.Unmarshal([]byte(entry.text), &args) == nil {
			for _, key := range []string{"command", "path", "app_name", "device_name", "device", "address", "project_path", "query", "category", "job_id", "stable_id", "camera_id", "device_id"} {
				if value, ok := args[key].(string); ok && value != "" {
					return "› " + label + " · " + toolSummaryLine(value)
				}
			}
		}
		return "› " + label
	}
	text := strings.TrimSpace(entry.text)
	var imageCount int
	if n, err := fmt.Sscanf(text, "[%d image(s) attached for visual inspection.]", &imageCount); err == nil && n == 1 && imageCount > 0 {
		if imageCount == 1 {
			return "↳ 1 image captured"
		}
		return fmt.Sprintf("↳ %d images captured", imageCount)
	}
	switch {
	case strings.HasPrefix(text, "Tool error:"):
		return "! " + toolFailureSummary(text)
	case strings.HasPrefix(text, "User denied permission"):
		return "↳ Denied"
	case strings.HasPrefix(text, "Tool was not executed"):
		return "↳ " + toolSummaryLine(strings.SplitN(text, "\n", 2)[0])
	case text == "":
		return "↳ No output"
	}
	var data any
	if json.Unmarshal([]byte(text), &data) == nil {
		return "↳ " + toolJSONSummary(data)
	}
	if strings.Contains(text, "\n") {
		return fmt.Sprintf("↳ %d lines returned", strings.Count(text, "\n")+1)
	}
	return "↳ " + toolSummaryLine(text)
}

func toolJSONSummary(data any) string {
	switch value := data.(type) {
	case []any:
		return fmt.Sprintf("%d items returned", len(value))
	case map[string]any:
		if code, _ := value["error_code"].(string); code != "" {
			return "Tool error: " + toolSummaryLine(toolErrorMessage(value))
		}
		if summary := backgroundJobSummary(value); summary != "" {
			return summary
		}
		if ok, exists := value["success"].(bool); exists && !ok {
			if detail := toolErrorMessage(value); detail != "" {
				return "Tool reported a failure · " + toolSummaryLine(detail)
			}
			return "Tool reported a failure · /tools for details"
		}
		if err, exists := value["error"]; exists && err != nil && err != "" && err != false {
			if detail := toolErrorMessage(value); detail != "" {
				return "Tool reported an error · " + toolSummaryLine(detail)
			}
			return "Tool reported an error · /tools for details"
		}
		if jobs, ok := value["jobs"].([]any); ok {
			if len(jobs) == 1 {
				if job, ok := jobs[0].(map[string]any); ok {
					if summary := backgroundJobSummary(job); summary != "" {
						return summary
					}
				}
			}
			counts := make(map[string]int)
			for _, job := range jobs {
				if job, ok := job.(map[string]any); ok {
					state, _ := job["state"].(string)
					counts[state]++
				}
			}
			summary := fmt.Sprintf("%d background jobs", len(jobs))
			if len(jobs) == 1 {
				summary = "1 background job"
			}
			var states []string
			for _, state := range []string{"running", "failed", "exited", "stopped"} {
				if counts[state] > 0 {
					states = append(states, fmt.Sprintf("%d %s", counts[state], state))
				}
			}
			if len(states) > 0 {
				summary += " · " + strings.Join(states, ", ")
			}
			return summary
		}
		for _, key := range []string{"devices", "containers", "cameras", "networks", "entries", "files", "tools", "batches"} {
			if list, ok := value[key].([]any); ok {
				return fmt.Sprintf("%d %s returned", len(list), key)
			}
		}
		if nested, ok := value["data"]; ok {
			if _, isMap := nested.(map[string]any); isMap {
				return toolJSONSummary(nested)
			}
		}
	}
	return "Result received"
}

func backgroundJobSummary(value map[string]any) string {
	id, _ := value["job_id"].(string)
	kind, _ := value["kind"].(string)
	state, _ := value["state"].(string)
	if id == "" || (kind != "camera_view" && kind != "audio_listen") {
		return ""
	}
	switch state {
	case "running", "exited", "failed", "stopped":
	default:
		return ""
	}
	parts := []string{strings.ReplaceAll(kind, "_", " "), state, id}
	if device, _ := value["device"].(string); device != "" {
		parts = append(parts, device)
	}
	if state == "failed" {
		detail, _ := value["error"].(string)
		if tail, _ := value["output_tail"].(string); tail != "" {
			if cause := toolFailureDetail(tail, true); cause != "" {
				detail = cause
			}
		}
		if detail != "" {
			parts = append(parts, detail)
		}
	}
	return toolSummaryLine(strings.Join(parts, " · "))
}

// Keep the full error in the transcript, but show its cause in the compact row.
// MCP errors have a generic wrapper; command failures put stdout/stderr below
// the exit status, with the final diagnostic usually at the end of that output.
func toolFailureSummary(text string) string {
	header, output, _ := strings.Cut(text, "\n")
	detail := toolFailureDetail(output, strings.HasPrefix(header, "Tool error: command failed:"))
	if detail == "" {
		return toolSummaryLine(header)
	}
	if header == "Tool error: Wendy MCP tool reported an error" {
		return toolSummaryLine("Tool error: " + detail)
	}
	return toolSummaryLine(header + " · " + detail)
}

func toolFailureDetail(output string, lastLine bool) string {
	var data map[string]any
	if json.Unmarshal([]byte(output), &data) == nil {
		if summary := backgroundJobSummary(data); summary != "" {
			return summary
		}
		if detail := toolErrorMessage(data); detail != "" {
			return detail
		}
	}
	var detail string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != "[Output truncated at 32 KiB.]" {
			detail = line
			if !lastLine {
				break
			}
		}
	}
	return detail
}

func toolErrorMessage(data map[string]any) string {
	if nested, ok := data["error"].(map[string]any); ok {
		if detail := toolErrorMessage(nested); detail != "" {
			return detail
		}
	}
	message, _ := data["message"].(string)
	if message == "" {
		message, _ = data["error"].(string)
	}
	code, _ := data["error_code"].(string)
	if code == "" {
		code, _ = data["code"].(string)
	}
	if code != "" {
		return strings.TrimSpace("[" + code + "] " + message)
	}
	return message
}

func toolSummaryLine(text string) string {
	return ansi.Truncate(strings.Join(strings.Fields(chatSanitize(text)), " "), 120, "…")
}
