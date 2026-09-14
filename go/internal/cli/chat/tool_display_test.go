package chat

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestToolDetailsAreCompactAndCanBeExpanded(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.transcript = nil
	call := ToolCall{Name: "cloud_discover", Arguments: json.RawMessage(`{"scan":true}`)}
	m.handleEvent(Event{Type: "tool_start", Call: &call})
	m.handleEvent(Event{Type: "tool_result", Call: &call, Text: `{"devices":[{"private_detail":"one"},{"private_detail":"two"}]}`})
	compact := ansi.Strip(m.viewport.View())
	if !strings.Contains(compact, "cloud discover") || !strings.Contains(compact, "2 devices returned") || strings.Contains(compact, "private_detail") || strings.Contains(compact, `"scan"`) {
		t.Fatalf("unexpected compact tool display: %s", compact)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	expanded := ansi.Strip(m.viewport.View())
	if !strings.Contains(expanded, "private_detail") || !strings.Contains(expanded, `"scan"`) {
		t.Fatalf("tool details were lost: %s", expanded)
	}
	m.submit("/tools")
	if strings.Contains(ansi.Strip(m.viewport.View()), "private_detail") {
		t.Fatal("/tools did not collapse the details")
	}
}

func TestCompactToolResultsPreserveFailures(t *testing.T) {
	for _, text := range []string{"Tool error: disconnected\ninternal details", `{"success":false,"error":{"code":"offline"}}`, "User denied permission. Tool was not executed."} {
		got := compactToolEntry(chatEntry{kind: "result", text: text})
		if !strings.Contains(got, "error") && !strings.Contains(got, "failure") && !strings.Contains(got, "Denied") {
			t.Fatalf("failure presented as a success: %s", got)
		}
	}
}
