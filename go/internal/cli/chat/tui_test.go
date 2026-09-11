package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type uiProviderFunc func(context.Context, []Message, []Tool, func(string)) (Message, error)

func (f uiProviderFunc) Stream(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
	return f(ctx, messages, tools, emit)
}

type uiExecutor struct {
	tools    []Tool
	executed atomic.Int32
}

func (e *uiExecutor) ListTools(context.Context) ([]Tool, error) { return e.tools, nil }

func (e *uiExecutor) Execute(context.Context, ToolCall) (string, error) {
	e.executed.Add(1)
	return "tool completed", nil
}

func uiModel(t *testing.T, provider Provider, executor Executor, autoApprove bool) *chatModel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := newChatModel(ctx, UIOptions{
		Engine: NewEngine(provider, executor, "test"), Model: "test-model", Provider: "test-provider",
		Workspace: "/workspace", Device: "raspberry-pi", AutoApprove: autoApprove,
	})
	t.Cleanup(func() {
		cancel()
		finished := make(chan struct{})
		go func() { m.workers.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("chat worker did not stop after canceling its session")
		}
	})
	return m
}

func uiNextEvent(t *testing.T, m *chatModel) turnMessage {
	t.Helper()
	select {
	case event, ok := <-m.events:
		if !ok {
			t.Fatal("event channel closed without turn completion")
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for agent event")
		return turnMessage{}
	}
}

func uiDrainTurn(t *testing.T, m *chatModel) {
	t.Helper()
	for m.active {
		m.Update(uiNextEvent(t, m))
	}
}

func uiApprovalProvider(call ToolCall) Provider {
	return uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		if messages[len(messages)-1].Role == "tool" {
			return Message{Content: "Finished."}, nil
		}
		return Message{ToolCalls: []ToolCall{call}}, nil
	})
}

func uiWaitForApproval(t *testing.T, m *chatModel) {
	t.Helper()
	for m.approval == nil {
		msg := uiNextEvent(t, m)
		if msg.done {
			t.Fatalf("turn ended before approval: %v", msg.err)
		}
		m.Update(msg)
	}
}

func TestUIStreamingPreservesEveryChunkBeforeCompletion(t *testing.T) {
	var want strings.Builder
	for i := 0; i < 256; i++ {
		fmt.Fprintf(&want, "%03d ", i)
	}
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		for i := 0; i < 256; i++ {
			emit(fmt.Sprintf("%03d ", i))
		}
		return Message{}, nil
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	m.startTurn("hello")
	uiDrainTurn(t, m)
	var got strings.Builder
	for _, entry := range m.transcript {
		if entry.kind == "assistant" {
			got.WriteString(entry.text)
		}
	}
	if got.String() != want.String() {
		t.Fatalf("stream output lost or reordered: got %d bytes, want %d", got.Len(), want.Len())
	}
	if m.status != "Ready" || m.cancelTurn != nil {
		t.Fatalf("completion did not reset active state: %+v", m.status)
	}
}

func TestUICancelActiveTurnThenExitWhenIdle(t *testing.T) {
	entered := make(chan struct{})
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		close(entered)
		<-ctx.Done()
		return Message{}, ctx.Err()
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	m.startTurn("wait")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil || !m.canceling || m.quitting {
		t.Fatal("Ctrl+C during a turn must cancel work and keep chat open")
	}
	uiDrainTurn(t, m)
	if !strings.Contains(m.transcript[len(m.transcript)-1].title, "Canceled") {
		t.Fatal("cancellation should be visible in the transcript")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("Ctrl+C while idle should exit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("idle Ctrl+C did not return a quit command")
	}
}

func TestUIToolApprovalAllowsDeniesAndAutoApproves(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
		auto bool
		want int32
	}{
		{name: "allow", key: "y", want: 1},
		{name: "deny", key: "n"},
		{name: "auto", auto: true, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := ToolCall{ID: "call-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"app.go","content":"package app"}`)}
			executor := &uiExecutor{tools: []Tool{{Name: call.Name, RequiresApproval: true}}}
			m := uiModel(t, uiApprovalProvider(call), executor, test.auto)
			m.startTurn("write the file")
			if !test.auto {
				uiWaitForApproval(t, m)
				if executor.executed.Load() != 0 {
					t.Fatal("mutating tool executed before approval")
				}
				if !strings.Contains(ansi.Strip(m.View()), "app.go") {
					t.Fatal("approval did not display tool arguments")
				}
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(test.key)})
			}
			uiDrainTurn(t, m)
			if got := executor.executed.Load(); got != test.want {
				t.Fatalf("tool executed %d times, want %d", got, test.want)
			}
		})
	}
}

func TestUICancelPendingApprovalUnblocksWorker(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "shell", Arguments: json.RawMessage(`{"command":"echo hello"}`)}
	executor := &uiExecutor{tools: []Tool{{Name: call.Name, RequiresApproval: true}}}
	m := uiModel(t, uiApprovalProvider(call), executor, false)
	m.startTurn("run command")
	uiWaitForApproval(t, m)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	uiDrainTurn(t, m)
	if m.approval != nil || executor.executed.Load() != 0 {
		t.Fatal("canceled approval must disappear without executing its tool")
	}
}

func TestUISessionExitUnblocksFullStreamQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queueFull := make(chan struct{})
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		for i := 0; ctx.Err() == nil; i++ {
			if i == 63 {
				// The status event plus the first 63 chunks fill the queue.
				close(queueFull)
			}
			emit("chunk ")
		}
		return Message{}, ctx.Err()
	})
	m := newChatModel(ctx, UIOptions{Engine: NewEngine(provider, &uiExecutor{}, "")})
	m.startTurn("stream")
	// Do not consume any events. Session cancellation must release a producer
	// blocked by the full queue and its final completion send.
	select {
	case <-queueFull:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not fill its event queue")
	}
	cancel()
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming worker leaked after session exit")
	}
}

func TestUIApprovalPreviewContainsFullMultilineContent(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	content := strings.Repeat("line of a script\n", 100) + "LAST_LINE_MARKER"
	raw, _ := json.Marshal(map[string]string{"path": "script.sh", "content": content})
	m.approval = &approvalRequest{call: ToolCall{Name: "write_file", Arguments: raw}, reply: make(chan bool, 1)}
	m.refreshApproval()
	if m.preview.TotalLineCount() <= m.preview.Height {
		t.Fatal("large approval should extend beyond its scrollable viewport")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if !strings.Contains(ansi.Strip(m.preview.View()), "LAST_LINE_MARKER") {
		t.Fatal("end of the complete proposed file must be reachable by scrolling")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlHome})
	if !m.preview.AtTop() {
		t.Fatal("Ctrl+Home should return to beginning of approval")
	}
}

func TestUIScrollingDuringStreamingDoesNotJumpToBottom(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.appendEntry("assistant", "Wendy", strings.Repeat("line\n", 100))
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	offset := m.viewport.YOffset
	if m.viewport.AtBottom() {
		t.Fatal("PgUp should scroll away from the end of the transcript")
	}
	m.handleEvent(Event{Type: "text", Text: "next streamed token"})
	if m.viewport.YOffset != offset {
		t.Fatal("a streamed token moved the reader's scroll position")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	m.handleEvent(Event{Type: "text", Text: strings.Repeat("\nmore text", 10)})
	if !m.viewport.AtBottom() {
		t.Fatal("Ctrl+End should resume following the streaming response")
	}
}

func TestUIResizeAndUntrustedLabelsStayWithinTerminal(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.opts.Device = "pi\n\n\x1b[2Jspoofed\u202e"
	m.opts.Workspace = strings.Repeat("long/path/", 100)
	m.appendEntry("assistant", "Wendy", strings.Repeat("Device status 硬件\n", 30))
	for _, size := range [][2]int{{120, 40}, {80, 24}, {40, 14}, {20, 10}, {5, 4}, {1, 1}, {0, 0}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View()
		if got := len(strings.Split(view, "\n")); got > max(1, size[1]) {
			t.Errorf("size %v: view has %d rows", size, got)
		}
		for _, line := range strings.Split(view, "\n") {
			if got := ansi.StringWidth(line); got > max(1, size[0]) {
				t.Errorf("size %v: line is %d columns wide", size, got)
			}
		}
		if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\u202e") {
			t.Errorf("size %v: untrusted label escaped sanitization", size)
		}
	}
}

func TestUIHeaderShowsDevicePreferenceWithoutClaimingLiveConnection(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	if !strings.Contains(ansi.Strip(m.View()), "Device preference raspberry-pi") {
		t.Fatal("explicit device should appear as a preference, not a live connection")
	}
	m.opts.Device = ""
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "Device preference configured default or discovery") {
		t.Fatal("empty device preference should account for configured default connections")
	}
}

func TestUISanitizesSplitStreamingEscapesAndPreservesNewlines(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.handleEvent(Event{Type: "text", Text: "before\x1b["})
	m.handleEvent(Event{Type: "text", Text: "31mred\x1b[0m\n\tafter\u202e\x00"})
	want := "beforered\n    after"
	if got := chatSanitize(m.transcript[len(m.transcript)-1].text); got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
	if got := chatSanitize("a\x1b]52;c;aGVsbG8=\x07b"); got != "ab" {
		t.Fatalf("OSC clipboard sequence was not stripped: %q", got)
	}
}

func TestUIComposerMultilineAndClearCommand(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first line")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second line")})
	if got := m.composer.Value(); got != "first line\nsecond line" {
		t.Fatalf("multiline composer = %q", got)
	}
	m.composer.SetValue("/clear")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.transcript) != 1 || m.transcript[0].title != "Conversation cleared" || m.composer.Value() != "" {
		t.Fatal("/clear did not reset the transcript and input")
	}
}

func TestUIStandaloneCredentialsAreRemovedBeforeRenderingOrSending(t *testing.T) {
	for _, token := range []string{
		"sk-this-is-an-obviously-fake-test-key",
		"sk-proj-obviously_fake_test_key_1234567890",
		"sk-ant-api03-obviously-fake-test-key-1234567890",
	} {
		for _, source := range []string{"paste", "active draft", "textarea update", "submit", "initial prompt"} {
			t.Run(source+"/"+token[:6], func(t *testing.T) {
				var requests atomic.Int32
				provider := uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
					requests.Add(1)
					return Message{}, nil
				})
				m := uiModel(t, provider, &uiExecutor{}, false)
				switch source {
				case "paste":
					m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("  " + token + "\n"), Paste: true})
				case "active draft":
					m.active = true
					m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(token), Paste: true})
				case "textarea update":
					// Clipboard paste completion reaches Update as a textarea
					// message, independently of the keyboard branch.
					m.composer.SetValue(token)
					m.Update(struct{}{})
				case "submit":
					m.composer.SetValue(token)
					m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				case "initial prompt":
					m.opts.InitialPrompt = token
					m.Update(initialPromptMessage{})
				}
				if m.composer.Value() != "" || m.opts.InitialPrompt != "" || m.turnID != 0 || m.events != nil || requests.Load() != 0 {
					t.Fatal("credential must be cleared without starting a model request or creating events")
				}
				view := ansi.Strip(m.View())
				if strings.Contains(view, token) || !strings.Contains(view, "Use /setup") {
					t.Fatal("view must show private setup guidance without the credential")
				}
				for _, entry := range m.transcript {
					if strings.Contains(entry.title+entry.text, token) {
						t.Fatal("credential reached the transcript")
					}
				}
				for _, message := range m.opts.Engine.Messages() {
					if strings.Contains(message.Content, token) {
						t.Fatal("credential reached conversation history")
					}
				}
			})
		}
	}
}

func TestUIInitialCredentialDiscardedBeforeFirstFrame(t *testing.T) {
	const token = "sk-proj-obviously_fake_initial_test_key_12345"
	m := newChatModel(context.Background(), UIOptions{InitialPrompt: token})
	if m.opts.InitialPrompt != "" || strings.Contains(m.View(), token) || m.events != nil {
		t.Fatal("initial credential must be discarded before rendering or initialization")
	}
	if !strings.Contains(ansi.Strip(m.View()), "Use /setup") {
		t.Fatal("initial credential must produce private setup guidance")
	}
}

func TestUICredentialDetectionLeavesProseAndCodeEditable(t *testing.T) {
	for _, value := range []string{
		"What does an sk-proj- prefix mean?",
		"Explain sk-this-is-an-obviously-fake-test-key please.",
		`key := "sk-this-is-an-obviously-fake-test-key"`,
		"sk-short-example",
		"package main\n// sk-this-is-an-obviously-fake-test-key",
	} {
		m := uiModel(t, nil, &uiExecutor{}, false)
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value), Paste: true})
		if m.composer.Value() != value || len(m.transcript) != 1 {
			t.Fatalf("ordinary prose or code was incorrectly removed: %q", value)
		}
	}
}

func TestUISetupCommandReturnsToConfiguration(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.composer.SetValue("/setup")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.reconfigure || !m.quitting || cmd == nil || m.events != nil || m.active {
		t.Fatal("/setup must leave chat without starting a model request")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("/setup must quit the current terminal session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := Run(ctx, UIOptions{
		Engine: NewEngine(nil, &uiExecutor{}, ""), InitialPrompt: "/setup",
		Input: strings.NewReader(""), Output: &bytes.Buffer{},
	})
	if !errors.Is(err, ErrReconfigure) {
		t.Fatalf("/setup returned %v, want ErrReconfigure", err)
	}
}

func TestUIHelpAdvertisesPrivateSetup(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.submit("/help")
	if !strings.Contains(m.transcript[len(m.transcript)-1].text, "/setup  Change your AI or enter an API key privately") {
		t.Fatal("chat help must explain private setup")
	}
}

func TestUIReconfigureRetainsTranscriptForResume(t *testing.T) {
	state := &UIState{transcript: []chatEntry{
		{kind: "user", title: "You", text: "Inspect my device"},
		{kind: "assistant", title: "Wendy", text: "The device is connected."},
	}}
	engine := NewEngine(nil, &uiExecutor{}, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, UIOptions{
		Engine: engine, State: state, InitialPrompt: "/setup",
		Input: strings.NewReader(""), Output: &bytes.Buffer{},
	})
	if !errors.Is(err, ErrReconfigure) {
		t.Fatalf("/setup returned %v", err)
	}
	resumed := newChatModel(ctx, UIOptions{Engine: engine, State: state})
	if len(resumed.transcript) != 2 || resumed.transcript[1].text != "The device is connected." {
		t.Fatalf("conversation disappeared after returning from setup: %#v", resumed.transcript)
	}
}
