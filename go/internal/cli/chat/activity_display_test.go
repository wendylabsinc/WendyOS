package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestActivityGroupsInterleavedAgentsAndRetainsDetails(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.transcript = nil
	m.active = true
	for i := 1; i <= 4; i++ {
		m.handleEvent(Event{Type: "agent_start", AgentID: fmt.Sprintf("agent-%d", i), Profile: "fleet", Text: strings.Repeat("long private task ", 100)})
	}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("agent-%d", i%4+1)
		call := ToolCall{Name: "camera_list"}
		m.handleEvent(Event{Type: "tool_start", AgentID: id, Profile: "fleet", Call: &call})
		m.handleEvent(Event{Type: "tool_result", AgentID: id, Profile: "fleet", Call: &call, Text: `{"cameras":[],"private_result":true}`})
	}
	for i := 1; i <= 4; i++ {
		m.handleEvent(Event{Type: "agent_done", AgentID: fmt.Sprintf("agent-%d", i), Profile: "fleet", Text: "completed: " + strings.Repeat("long private report\n", 100)})
	}
	compact, _ := m.transcriptContent(100)
	compact = ansi.Strip(compact)
	if strings.Count(compact, "\n") != 0 || !strings.Contains(compact, "40 tools · 4/4 agents finished") || strings.Contains(compact, "private") {
		t.Fatalf("activity grew beyond one summary row: %q", compact)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	expanded, _ := m.transcriptContent(100)
	for _, want := range []string{"long private task", "private_result", "long private report"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded activity lost %q", want)
		}
	}
	m.submit("/tools")
	m.handleEvent(Event{Type: "text", Text: "The final answer stays visible."})
	content, _ := m.transcriptContent(100)
	if !strings.Contains(content, "The final answer stays visible.") || strings.Count(content, "\n") != 3 {
		t.Fatalf("answer was folded into activity: %q", content)
	}
}

func TestActivityKeepsEarlierFailuresVisible(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.transcript = []chatEntry{
		{kind: "result", title: "Result · cloud_connect", text: "Tool error: offline"},
		{kind: "result", title: "Result · shell", text: "User denied permission."},
		{kind: "agent_done", title: "agent-1 · fleet", text: "failed: missing credentials"},
		{kind: "tool", title: "Tool · device_list"},
		{kind: "result", title: "Result · device_list", text: `{"devices":[]}`},
	}
	content, _ := m.transcriptContent(100)
	for _, want := range []string{"offline", "Denied", "missing credentials"} {
		if !strings.Contains(content, want) {
			t.Fatalf("later success hid %q: %s", want, content)
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if ansi.StringWidth(line) > 100 {
			t.Fatalf("activity overflowed width: %q", line)
		}
	}
}
