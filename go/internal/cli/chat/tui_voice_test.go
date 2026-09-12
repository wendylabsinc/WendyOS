package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type uiVoiceReply struct{ id, text string }

type uiVoiceSession struct {
	events      chan VoiceEvent
	replies     chan uiVoiceReply
	contexts    chan string
	closed      chan struct{}
	closeOnce   sync.Once
	interrupts  atomic.Int32
	contextGate <-chan struct{}
}

func newUIVoiceSession() *uiVoiceSession {
	return &uiVoiceSession{
		events: make(chan VoiceEvent, 32), replies: make(chan uiVoiceReply, 32),
		contexts: make(chan string, 32), closed: make(chan struct{}),
	}
}

func (s *uiVoiceSession) Events() <-chan VoiceEvent { return s.events }
func (s *uiVoiceSession) SendContext(ctx context.Context, text string) error {
	if s.contextGate != nil {
		select {
		case <-s.contextGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case s.contexts <- text:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *uiVoiceSession) Reply(ctx context.Context, id, text string) error {
	select {
	case s.replies <- uiVoiceReply{id: id, text: text}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *uiVoiceSession) Interrupt(context.Context) error {
	s.interrupts.Add(1)
	return nil
}
func (s *uiVoiceSession) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func uiNextVoiceEvent(t *testing.T, m *chatModel) voiceMessage {
	t.Helper()
	select {
	case event := <-m.voiceEvents:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for voice event")
		return voiceMessage{}
	}
}

func uiSendVoice(t *testing.T, m *chatModel, session *uiVoiceSession, event VoiceEvent) {
	t.Helper()
	session.events <- event
	m.Update(uiNextVoiceEvent(t, m))
}

func uiConnectVoice(t *testing.T, m *chatModel) *uiVoiceSession {
	t.Helper()
	session := newUIVoiceSession()
	m.opts.VoiceFactory = func(context.Context) (VoiceSession, error) { return session, nil }
	m.startVoice()
	m.Update(uiNextVoiceEvent(t, m))
	uiSendVoice(t, m, session, VoiceEvent{Type: "ready"})
	if !m.voiceEnabled || m.voiceStarting || m.voiceSession == nil {
		t.Fatal("voice did not transition to listening")
	}
	select {
	case reply := <-session.replies:
		t.Fatalf("voice must not speak an initial greeting: %+v", reply)
	default:
	}
	return session
}

func uiNextVoiceReply(t *testing.T, session *uiVoiceSession) uiVoiceReply {
	t.Helper()
	select {
	case reply := <-session.replies:
		return reply
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for spoken reply")
		return uiVoiceReply{}
	}
}

func uiVoiceStopAndWait(t *testing.T, m *chatModel, session *uiVoiceSession) {
	t.Helper()
	m.stopVoice()
	select {
	case <-session.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("voice connection and microphone were not closed")
	}
	m.workers.Wait()
}

func TestUIVoiceDelegationUsesEngineApprovalsAndOnlySpeaksFinalResult(t *testing.T) {
	for _, decision := range []string{"y", "n"} {
		t.Run(decision, func(t *testing.T) {
			call := ToolCall{ID: "write", Name: "workspace_write", Arguments: json.RawMessage(`{"path":"app.txt","content":"hello"}`)}
			executor := &uiExecutor{tools: []Tool{{Name: call.Name, RequiresApproval: true}}}
			provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
				if messages[len(messages)-1].Role == "tool" {
					text := "Created app.txt."
					if strings.Contains(messages[len(messages)-1].Content, "denied") {
						text = "The file was not changed."
					}
					emit(text)
					return Message{Content: text}, nil
				}
				emit("I will first write the file and check its contents.")
				return Message{ToolCalls: []ToolCall{call}}, nil
			})
			m := uiModel(t, provider, executor, false)
			session := uiConnectVoice(t, m)
			uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "task-1", Text: "Create app.txt"})
			uiWaitForApproval(t, m)
			approval := uiNextVoiceReply(t, session)
			if approval.id != "" || !strings.Contains(approval.text, "press y or n") {
				t.Fatalf("expected a short approval notice without completing the task, got %+v", approval)
			}
			// An affirmative spoken transcript can never authorize the file write.
			uiSendVoice(t, m, session, VoiceEvent{Type: "input", Text: "yes, approved"})
			if m.approval == nil || executor.executed.Load() != 0 {
				t.Fatal("spoken words bypassed keyboard approval")
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(decision)})
			uiDrainTurn(t, m)
			final := uiNextVoiceReply(t, session)
			want, runs := "Created app.txt.", int32(1)
			if decision == "n" {
				want, runs = "The file was not changed.", 0
			}
			if final.id != "task-1" || final.text != want || executor.executed.Load() != runs {
				t.Fatalf("final reply=%+v, tool executions=%d; want %q, %d", final, executor.executed.Load(), want, runs)
			}
			uiVoiceStopAndWait(t, m, session)
			if len(session.replies) != 0 {
				t.Fatal("tool progress was spoken or final result was spoken more than once")
			}
		})
	}
}

func TestUIVoiceCorrectionCancelsOldTurnBeforeStartingNewAndDropsLateResult(t *testing.T) {
	oldStarted := make(chan struct{})
	var calls atomic.Int32
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		if calls.Add(1) == 1 {
			close(oldStarted)
			<-ctx.Done()
			// Deliberately return a late completion to model a provider race.
			return Message{Content: "STALE result from the first request"}, nil
		}
		if !strings.Contains(messages[len(messages)-1].Content, "corrected task") {
			t.Error("the replacement request did not reach the backend engine")
		}
		return Message{Content: "Corrected task finished."}, nil
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	session := uiConnectVoice(t, m)
	uiSendVoice(t, m, session, VoiceEvent{Type: "input", Text: "first task"})
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "old", Text: "first task"})
	select {
	case <-oldStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first backend turn did not start")
	}
	uiSendVoice(t, m, session, VoiceEvent{Type: "input", Text: "corrected task"})
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "new", Text: "corrected task"})
	if !m.canceling || m.pendingDelegation == nil || calls.Load() != 1 {
		t.Fatal("replacement turn must wait for cancellation of the old turn")
	}
	uiDrainTurn(t, m)
	final := uiNextVoiceReply(t, session)
	if final.id != "new" || final.text != "Corrected task finished." {
		t.Fatalf("stale result was spoken: %+v", final)
	}
	// Duplicate events from the live transport must not run a task twice.
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "new", Text: "corrected task"})
	if m.active || calls.Load() != 2 {
		t.Fatal("a duplicate delegation executed another backend turn")
	}
	counts := map[string]int{}
	for _, entry := range m.transcript {
		if entry.kind == "user" || entry.kind == "voice_input" {
			counts[entry.text]++
		}
	}
	if counts["first task"] != 1 || counts["corrected task"] != 1 {
		t.Fatalf("correction should promote its own live caption exactly once: %#v", counts)
	}
	uiVoiceStopAndWait(t, m, session)
	if len(session.replies) != 0 {
		t.Fatal("the canceled task produced a late spoken result")
	}
}

func TestUIVoiceTypedMessagesRemainUsableAndContextOnly(t *testing.T) {
	provider := uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		return Message{Content: "Typed answer."}, nil
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	session := uiConnectVoice(t, m)
	m.submit("Typed question")
	uiDrainTurn(t, m)
	var contexts []string
	for len(contexts) < 3 {
		select {
		case text := <-session.contexts:
			contexts = append(contexts, text)
		case <-time.After(3 * time.Second):
			t.Fatal("typed turn did not finish sending voice context")
		}
	}
	uiVoiceStopAndWait(t, m, session)
	joined := strings.Join(contexts, "\n")
	if !strings.Contains(joined, "Typed question") || !strings.Contains(joined, "Typed answer.") {
		t.Fatalf("typed conversation was not mirrored as context: %q", joined)
	}
	if len(session.replies) != 0 {
		t.Fatal("typed messages should update context without unsolicited narration")
	}
	if len(m.opts.Engine.Messages()) != 3 {
		t.Fatal("turning voice off must preserve the text conversation")
	}
	if !strings.Contains(ansi.Strip(m.View()), "mic muted") {
		t.Fatal("voice-off state needs a visible muted indicator")
	}
}

func TestUIVoiceFailureFallsBackToTextAndClosesSession(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	session := uiConnectVoice(t, m)
	uiSendVoice(t, m, session, VoiceEvent{Type: "error", Err: errors.New("connection lost")})
	if m.voiceEnabled || m.quitting || !strings.Contains(ansi.Strip(m.View()), "Text chat remains available") {
		t.Fatal("voice failure must leave a usable text session")
	}
	select {
	case <-session.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("failed voice session was not closed")
	}
}

func TestUIVoiceSetupExitsPrivatelyAndPreservesConversation(t *testing.T) {
	state := &UIState{transcript: []chatEntry{{kind: "assistant", title: "Wendy", text: "Existing conversation"}}, composer: "unfinished draft"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := Run(ctx, UIOptions{
		Engine: NewEngine(nil, &uiExecutor{}, ""), State: state, InitialPrompt: "/voice",
		Input: strings.NewReader(""), Output: &bytes.Buffer{},
	})
	if !errors.Is(err, ErrVoiceSetup) || state.Voice || state.composer != "unfinished draft" || len(state.transcript) != 1 {
		t.Fatalf("voice setup lost state or enabled voice before setup: err=%v state=%+v", err, state)
	}
}

func TestUIVoiceEscapeInterruptsPlaybackAndCancelsEngine(t *testing.T) {
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		<-ctx.Done()
		return Message{}, ctx.Err()
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	session := uiConnectVoice(t, m)
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "task", Text: "wait"})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	uiDrainTurn(t, m)
	deadline := time.After(3 * time.Second)
	for session.interrupts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("Escape did not stop live playback")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if m.active || !m.voiceEnabled || m.quitting {
		t.Fatal("Escape should stop active work and keep the conversation available")
	}
	uiVoiceStopAndWait(t, m, session)
	if len(session.replies) != 0 {
		t.Fatal("interrupted voice task must not speak a result")
	}
}

func TestUIVoiceFactoryCancellationDoesNotLeak(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	started, stopped := make(chan struct{}), make(chan struct{})
	m.opts.VoiceFactory = func(ctx context.Context) (VoiceSession, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	m.startVoice()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("voice factory did not start")
	}
	m.submit("/voice off")
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("voice setup did not cancel when toggled off")
	}
	if m.voiceEnabled || m.quitting {
		t.Fatal("turning off connecting voice should leave text chat open")
	}
}

func TestUIVoiceCaptionsCoalesceAndClearDiscardsVoiceContext(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	session := uiConnectVoice(t, m)
	before := len(m.transcript)
	for _, text := range []string{"Check", " my", " device."} {
		uiSendVoice(t, m, session, VoiceEvent{Type: "input", Text: text})
	}
	if len(m.transcript) != before+1 || m.transcript[len(m.transcript)-1].text != "Check my device." {
		t.Fatal("consecutive voice transcript deltas must form one readable caption")
	}
	m.submit("/clear")
	if m.voiceEnabled || len(m.transcript) != 1 || !strings.Contains(m.transcript[0].text, "previous context is discarded") {
		t.Fatal("/clear must discard voice context as well as the text conversation")
	}
	select {
	case <-session.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("/clear left the previous voice session alive")
	}
}

func TestUIVoiceOffKeepsDelegatedWorkAndApprovalsInText(t *testing.T) {
	call := ToolCall{ID: "write", Name: "workspace_write", Arguments: json.RawMessage(`{"path":"app.txt","content":"hello"}`)}
	executor := &uiExecutor{tools: []Tool{{Name: call.Name, RequiresApproval: true}}}
	m := uiModel(t, uiApprovalProvider(call), executor, false)
	session := uiConnectVoice(t, m)
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "task", Text: "write app.txt"})
	uiWaitForApproval(t, m)
	_ = uiNextVoiceReply(t, session) // The short keyboard-approval notice.
	m.submit("/voice off")
	if !m.active || m.canceling || m.approval == nil || m.voiceEnabled {
		t.Fatal("turning voice off must retain the running task and its pending approval")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	uiDrainTurn(t, m)
	uiVoiceStopAndWait(t, m, session)
	if executor.executed.Load() != 1 || !strings.Contains(ansi.Strip(m.View()), "Finished.") {
		t.Fatal("task did not finish normally in text after voice was disabled")
	}
	if len(session.replies) != 0 {
		t.Fatal("voice-off task completion must remain in text without speech")
	}
}

func TestUIVoiceQueuedResultIsDiscardedWhenAnotherDelegationArrives(t *testing.T) {
	var calls atomic.Int32
	provider := uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		if calls.Add(1) == 1 {
			return Message{Content: "Old result queued for speech."}, nil
		}
		return Message{Content: "Current result."}, nil
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	session := newUIVoiceSession()
	gate := make(chan struct{})
	session.contextGate = gate
	m.opts.VoiceFactory = func(context.Context) (VoiceSession, error) { return session, nil }
	m.startVoice()
	m.Update(uiNextVoiceEvent(t, m))
	uiSendVoice(t, m, session, VoiceEvent{Type: "ready"})
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "old", Text: "old task"})
	uiDrainTurn(t, m)
	uiSendVoice(t, m, session, VoiceEvent{Type: "delegation", DelegationID: "new", Text: "new task"})
	uiDrainTurn(t, m)
	close(gate)
	final := uiNextVoiceReply(t, session)
	if final.id != "new" || final.text != "Current result." {
		t.Fatalf("expired queued speech was sent after a correction: %+v", final)
	}
	uiVoiceStopAndWait(t, m, session)
	if len(session.replies) != 0 {
		t.Fatal("old queued result was spoken after the corrected task")
	}
}

func TestUIVoiceBackendContextIsNotDisplayedAsUserSpeech(t *testing.T) {
	received := make(chan string, 1)
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		received <- messages[len(messages)-1].Content
		return Message{Content: "Done."}, nil
	})
	m := uiModel(t, provider, &uiExecutor{}, false)
	session := uiConnectVoice(t, m)
	uiSendVoice(t, m, session, VoiceEvent{
		Type: "delegation", DelegationID: "task", Text: "Inspect my device",
		Prompt: `{"internal_context_marker":"Inspect my device with prior context"}`,
	})
	uiDrainTurn(t, m)
	if prompt := <-received; !strings.Contains(prompt, "internal_context_marker") {
		t.Fatal("full delegation context did not reach the backend")
	}
	for _, entry := range m.transcript {
		if strings.Contains(entry.text, "internal_context_marker") {
			t.Fatal("internal delegation JSON appeared in the conversation display")
		}
	}
	_ = uiNextVoiceReply(t, session)
	uiVoiceStopAndWait(t, m, session)
}

func TestUICompactToolLabelIsSanitized(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.transcript = nil
	m.handleEvent(Event{Type: "tool_start", Call: &ToolCall{Name: "read\n\x1b[2Jfile\u202e", Arguments: json.RawMessage(`{}`)}})
	view := m.viewport.View()
	if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\u202e") {
		t.Fatal("compact tool label retained untrusted terminal control sequences")
	}
}

func TestUIVoiceDelegationPromotesCaptionAndExecutesOnce(t *testing.T) {
	const question = "Which devices are online"
	call := ToolCall{ID: "list", Name: "device_list", Arguments: json.RawMessage(`{}`)}
	executor := &uiExecutor{tools: []Tool{{Name: call.Name}}}
	provider := uiProviderFunc(func(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		if messages[len(messages)-1].Role == "tool" {
			return Message{Content: "Two devices are online."}, nil
		}
		return Message{ToolCalls: []ToolCall{call}}, nil
	})
	m := uiModel(t, provider, executor, false)
	session := uiConnectVoice(t, m)
	uiSendVoice(t, m, session, VoiceEvent{Type: "input", Text: question})
	uiSendVoice(t, m, session, VoiceEvent{Type: "output", Text: "I'll"})
	delegation := VoiceEvent{Type: "delegation", DelegationID: "first", Text: question}
	uiSendVoice(t, m, session, delegation)
	for {
		event := uiNextEvent(t, m)
		m.Update(event)
		if event.event != nil && event.event.Type == "tool_start" {
			break
		}
	}
	// Reproduce the reported interleaving: audio text arrives both before and
	// after delegation/tool entries, but belongs to one spoken caption.
	uiSendVoice(t, m, session, VoiceEvent{Type: "output", Text: " check the device list."})
	uiSendVoice(t, m, session, delegation)
	uiDrainTurn(t, m)
	_ = uiNextVoiceReply(t, session)
	uiSendVoice(t, m, session, VoiceEvent{Type: "output", Text: "Two devices are online."})
	uiSendVoice(t, m, session, delegation)
	users, voice := 0, []string{}
	for _, entry := range m.transcript {
		if (entry.kind == "user" || entry.kind == "voice_input") && entry.text == question {
			users++
		}
		if entry.kind == "voice_output" {
			voice = append(voice, entry.text)
		}
	}
	engineUsers := 0
	for _, message := range m.opts.Engine.Messages() {
		if message.Role == "user" {
			engineUsers++
		}
	}
	if users != 1 || engineUsers != 1 || executor.executed.Load() != 1 || m.active {
		t.Fatalf("one spoken request was duplicated: captions=%d engine turns=%d executions=%d active=%v", users, engineUsers, executor.executed.Load(), m.active)
	}
	if len(voice) != 2 || voice[0] != "I'll check the device list." || voice[1] != "Two devices are online." {
		t.Fatalf("voice captions were fragmented or final result merged into progress: %#v", voice)
	}
	// Identical words spoken again are a distinct request, with their own
	// caption and delegation. Deduplication must never be global by text.
	uiSendVoice(t, m, session, VoiceEvent{Type: "input", Text: question})
	delegation.DelegationID = "second"
	uiSendVoice(t, m, session, delegation)
	uiDrainTurn(t, m)
	_ = uiNextVoiceReply(t, session)
	users = 0
	for _, entry := range m.transcript {
		if entry.kind == "user" && entry.text == question {
			users++
		}
	}
	if users != 2 || executor.executed.Load() != 2 {
		t.Fatalf("distinct repeated speech was dropped: captions=%d executions=%d", users, executor.executed.Load())
	}
	uiVoiceStopAndWait(t, m, session)
}
