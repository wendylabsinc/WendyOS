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
		label := strings.ReplaceAll(name, "_", " ")
		var args map[string]any
		if json.Unmarshal([]byte(entry.text), &args) == nil {
			for _, key := range []string{"command", "path", "app_name", "device_name", "device", "address", "project_path", "query", "category"} {
				if value, ok := args[key].(string); ok && value != "" {
					return "› " + label + " · " + toolSummaryLine(value)
				}
			}
		}
		return "› " + label
	}
	text := strings.TrimSpace(entry.text)
	switch {
	case strings.HasPrefix(text, "Tool error:"):
		return "! " + toolSummaryLine(strings.SplitN(text, "\n", 2)[0])
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
		if ok, exists := value["success"].(bool); exists && !ok {
			return "Tool reported a failure · /tools for details"
		}
		if err, exists := value["error"]; exists && err != nil && err != "" && err != false {
			return "Tool reported an error · /tools for details"
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

func toolSummaryLine(text string) string {
	return ansi.Truncate(strings.Join(strings.Fields(chatSanitize(text)), " "), 120, "…")
}
