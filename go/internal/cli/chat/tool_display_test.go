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

func TestCompactToolFailuresShowCause(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "MCP text fallback",
			text: "Tool error: Wendy MCP tool reported an error\n[NOT_FOUND] container go2-remote-control is not running\nStart the container first.\n",
			want: "! Tool error: [NOT_FOUND] container go2-remote-control is not running",
		},
		{
			name: "MCP structured error",
			text: "Tool error: Wendy MCP tool reported an error\n{\n  \"error_code\": \"NOT_CONNECTED\",\n  \"message\": \"Connect to a device first\"\n}",
			want: "! Tool error: [NOT_CONNECTED] Connect to a device first",
		},
		{
			name: "shell final diagnostic",
			text: "Tool error: command failed: exit status 1\nConnecting to Woof…\nPermission denied\n\n",
			want: "! Tool error: command failed: exit status 1 · Permission denied",
		},
		{
			name: "truncated shell output",
			text: "Tool error: command failed: exit status 1\nConnection refused\n[Output truncated at 32 KiB.]",
			want: "! Tool error: command failed: exit status 1 · Connection refused",
		},
		{
			name: "failure without output",
			text: "Tool error: command failed: context deadline exceeded\n",
			want: "! Tool error: command failed: context deadline exceeded",
		},
		{
			name: "direct structured Wendy failure",
			text: `{"error_code":"UNSUPPORTED","message":"Container attach is unavailable"}`,
			want: "↳ Tool error: [UNSUPPORTED] Container attach is unavailable",
		},
		{
			name: "nested structured failure",
			text: `{"success":false,"error":{"code":"offline","message":"Device Woof is offline"}}`,
			want: "↳ Tool reported a failure · [offline] Device Woof is offline",
		},
		{
			name: "structured string failure",
			text: `{"error":"Device Woof is offline"}`,
			want: "↳ Tool reported an error · Device Woof is offline",
		},
		{
			name: "structured failure message",
			text: `{"success":false,"message":"Device Woof is offline"}`,
			want: "↳ Tool reported a failure · Device Woof is offline",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := chatEntry{kind: "result", text: tt.text}
			if got := compactToolEntry(entry); got != tt.want {
				t.Fatalf("compact failure = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompactToolFailureCauseIsSanitizedAndBounded(t *testing.T) {
	text := "Tool error: Wendy MCP tool reported an error\n\x1b[31mConnection refused\x1b[0m " + strings.Repeat("details ", 100)
	got := compactToolEntry(chatEntry{kind: "result", text: text})
	if !strings.Contains(got, "Connection refused") || strings.ContainsAny(got, "\n\x1b") || ansi.StringWidth(got) > 122 {
		t.Fatalf("unsafe or oversized compact error: %q", got)
	}
}

func TestCompactBackgroundJobsShowState(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"camera running", `{"job_id":"job-1","kind":"camera_view","state":"running","device":"Woof","pid":42,"output_tail":"private diagnostic"}`, "↳ camera view · running · job-1 · Woof"},
		{"audio stopped", `{"job_id":"job-2","kind":"audio_listen","state":"stopped"}`, "↳ audio listen · stopped · job-2"},
		{"audio failed", `{"job_id":"job-2","kind":"audio_listen","state":"failed","error":"exit status 1","output_tail":"Starting playback\nAudio device unavailable\n"}`, "↳ audio listen · failed · job-2 · Audio device unavailable"},
		{"audio failed without output", `{"job_id":"job-2","kind":"audio_listen","state":"failed","error":"exit status 1"}`, "↳ audio listen · failed · job-2 · exit status 1"},
		{"camera exited", `{"job_id":"job-1","kind":"camera_view","state":"exited"}`, "↳ camera view · exited · job-1"},
		{"job list", `{"jobs":[{"state":"failed"},{"state":"running"},{"state":"stopped"}]}`, "↳ 3 background jobs · 1 running, 1 failed, 1 stopped"},
		{"one job", `{"jobs":[{"state":"running"}]}`, "↳ 1 background job · 1 running"},
		{"selected failed job", `{"jobs":[{"job_id":"job-2","kind":"audio_listen","state":"failed","error":"exit status 1","output_tail":"Audio device unavailable\n"}]}`, "↳ audio listen · failed · job-2 · Audio device unavailable"},
		{"startup failure", "Tool error: audio_listen failed\n{\"job_id\":\"job-2\",\"kind\":\"audio_listen\",\"state\":\"failed\",\"error\":\"exit status 1\"}", "! Tool error: audio_listen failed · audio listen · failed · job-2 · exit status 1"},
		{"empty list", `{"jobs":[]}`, "↳ 0 background jobs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactToolEntry(chatEntry{kind: "result", text: tt.text}); got != tt.want {
				t.Fatalf("compact job = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompactBackgroundJobDetailsAreSanitizedAndBounded(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"job_id": "job-1", "kind": "camera_view", "state": "failed",
		"device": "Woof\n\x1b[31m", "output_tail": "Decoder failed: " + strings.Repeat("detail ", 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := compactToolEntry(chatEntry{kind: "result", text: string(data)})
	if !strings.Contains(got, "failed") || !strings.Contains(got, "Decoder failed") || strings.ContainsAny(got, "\n\x1b") || ansi.StringWidth(got) > 122 {
		t.Fatalf("unsafe or oversized compact job: %q", got)
	}
}
