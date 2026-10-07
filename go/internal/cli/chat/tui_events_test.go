package chat

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type fakeWatchControl struct {
	notices chan WatchNotice
	listed  []WatchInfo
	stopped atomic.Int32
}

func (f *fakeWatchControl) WatchNotices() <-chan WatchNotice { return f.notices }
func (f *fakeWatchControl) ListWatches(context.Context) ([]WatchInfo, error) {
	return f.listed, nil
}
func (f *fakeWatchControl) StopAllWatches(context.Context) (int, error) {
	f.stopped.Add(1)
	return 1, nil
}

// uiWatchModel is uiModel with watches and a fixed clock the test can move.
func uiWatchModel(t *testing.T, provider Provider, executor Executor) (*chatModel, *fakeWatchControl, *time.Time) {
	t.Helper()
	control := &fakeWatchControl{notices: make(chan WatchNotice, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	m := newChatModel(ctx, UIOptions{Engine: NewEngine(provider, executor, "test"), Model: "test-model", Provider: "test-provider", Workspace: "/workspace", Watches: control})
	now := time.Date(2026, 10, 7, 14, 2, 11, 0, time.Local)
	m.now = func() time.Time { return now }
	t.Cleanup(func() {
		cancel()
		m.workers.Wait()
	})
	return m, control, &now
}

func watchStatus(state, reason string) WatchNotice {
	return WatchNotice{Method: watchStatusMethod, WatchID: "w1", Label: "front door", State: state, Reason: reason, Camera: "Brio 101", Watching: []string{"person"}}
}

func watchEntered(score float64) WatchNotice {
	return WatchNotice{Method: watchEventMethod, WatchID: "w1", Label: "front door", Sequence: 1, Kind: "entered", Classes: []WatchClass{{Label: "person", Score: score}}, OccurredAt: time.Date(2026, 10, 7, 14, 2, 11, 0, time.Local).UTC().Format(time.RFC3339Nano)}
}

func lastEntry(m *chatModel) chatEntry { return m.transcript[len(m.transcript)-1] }

func TestUIWatchNoticesShowAsEventLines(t *testing.T) {
	m, _, _ := uiWatchModel(t, uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		return Message{Content: "ok"}, nil
	}), &uiExecutor{})
	m.Update(watchNoticeMessage{watchStatus("PREPARING", "")})
	if e := lastEntry(m); e.kind != "event" || e.text != "· 14:02:11  front door  preparing" {
		t.Fatalf("status line %+v", e)
	}
	if view := ansi.Strip(m.View()); !strings.Contains(view, "watching: 1") {
		t.Fatalf("status bar lacks the watch count:\n%s", view)
	}
	m.Update(watchNoticeMessage{WatchNotice{Method: watchEventMethod, WatchID: "w1", Label: "front door", Kind: "left", OccurredAt: watchEntered(0).OccurredAt}})
	if e := lastEntry(m); e.text != "· 14:02:11  front door  detection left" {
		t.Fatalf("left line %+v", e)
	}
	m.Update(watchNoticeMessage{watchStatus("ENDED", "device changed")})
	if e := lastEntry(m); !strings.HasSuffix(e.text, "front door  ended: device changed") {
		t.Fatalf("ended line %+v", e)
	}
	if view := ansi.Strip(m.View()); strings.Contains(view, "watching:") {
		t.Fatal("an ended watch is still counted")
	}
}

func TestUIMissedWatchNoticesAreShown(t *testing.T) {
	m, _, _ := uiWatchModel(t, nil, &uiExecutor{})
	notice := watchStatus("PREPARING", "")
	notice.Missed, notice.MissedByServer = 2, 1
	m.Update(watchNoticeMessage{notice})
	found := false
	for _, e := range m.transcript {
		found = found || strings.Contains(e.text, "3 watch notification(s) were missed")
	}
	if !found {
		t.Fatal("dropped notices were not reported")
	}
}

func TestUIWatchesCommandsWorkWithoutTheModel(t *testing.T) {
	provider := uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		t.Fatal("a /watches command reached the model")
		return Message{}, nil
	})
	m, control, _ := uiWatchModel(t, provider, &uiExecutor{})
	control.listed = []WatchInfo{{WatchID: "w1", Label: "front door", CameraName: "Brio 101", Classes: []string{"person"}, State: "READY"}}
	cmd := m.submit("/watches")
	if cmd == nil {
		t.Fatal("/watches returned no command")
	}
	m.Update(cmd())
	if e := lastEntry(m); e.title != "Watches" || !strings.Contains(e.text, "w1  front door  person · Brio 101  READY") {
		t.Fatalf("listing %+v", e)
	}
	cmd = m.submit("/watches stop all")
	m.Update(cmd())
	if control.stopped.Load() != 1 || !strings.Contains(lastEntry(m).text, "Stopped 1 watch") {
		t.Fatalf("stop all: %+v", lastEntry(m))
	}
	if cmd := m.submit("/watches stop garage"); cmd != nil || lastEntry(m).title != "Unknown command" {
		t.Fatalf("an unknown /watches form must not reach the model: %+v", lastEntry(m))
	}
	m.submit("/help")
	if !strings.Contains(lastEntry(m).text, "/watches stop all") {
		t.Fatal("/help does not list /watches")
	}
}

func TestUIStateNoticeAppearsWhenChatReopens(t *testing.T) {
	state := new(UIState)
	state.AddNotice("Watches ended", "Setup restarts Wendy's device connection, so 2 camera watch(es) ended.")
	m := newChatModel(context.Background(), UIOptions{State: state})
	if len(m.transcript) != 2 || m.transcript[0].kind != "welcome" || m.transcript[1].title != "Watches ended" {
		t.Fatalf("transcript %+v", m.transcript)
	}
	var _ tea.Model = m
}

func TestUIStateCarriesWatchesAcrossRuns(t *testing.T) {
	state := new(UIState)
	provider := uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		return Message{Content: "ok"}, nil
	})
	control := &fakeWatchControl{notices: make(chan WatchNotice, 8)}
	newModel := func() (*chatModel, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		return newChatModel(ctx, UIOptions{Engine: NewEngine(provider, &uiExecutor{}, "test"), State: state, Watches: control}), cancel
	}
	first, cancel := newModel()
	first.Update(watchNoticeMessage{watchStatus("READY", "")})
	first.watches["w1"].startState = "PREPARING"
	first.watches["w1"].errorTurned = true
	first.lastWatchTurn = time.Date(2026, 10, 7, 14, 0, 0, 0, time.Local)
	cancel()
	first.workers.Wait()
	first.saveState(state)

	second, cancel2 := newModel()
	defer cancel2()
	if view := ansi.Strip(second.View()); !strings.Contains(view, "watching: 1") {
		t.Fatalf("watch count lost across runs:\n%s", view)
	}
	d := second.watches["w1"]
	if d == nil || d.startState != "PREPARING" || !d.errorTurned || second.lastWatchTurn.IsZero() {
		t.Fatalf("trigger memory lost: %+v", d)
	}
}

func TestUIWatchLineDoesNotSplitStreamingReply(t *testing.T) {
	m, _, _ := uiWatchModel(t, nil, &uiExecutor{})
	m.active = true
	m.handleEvent(Event{Type: "text", Text: "The front door camera is "})
	m.Update(watchNoticeMessage{watchEntered(0.9)})
	m.handleEvent(Event{Type: "text", Text: "now being watched."})
	assistants, events := 0, 0
	for _, e := range m.transcript {
		switch e.kind {
		case "assistant":
			assistants++
			if e.text != "The front door camera is now being watched." {
				t.Fatalf("reply text %q", e.text)
			}
		case "event":
			events++
		}
	}
	if assistants != 1 || events != 1 {
		t.Fatalf("assistants %d events %d: %+v", assistants, events, m.transcript)
	}
	if lastEntry(m).kind != "event" {
		t.Fatal("the event line must stay where it landed")
	}
}

func TestUIWatchLineKeepsRunningToolGroupRunning(t *testing.T) {
	m, _, _ := uiWatchModel(t, nil, &uiExecutor{})
	m.active = true
	call := ToolCall{Name: "camera_list"}
	m.handleEvent(Event{Type: "tool_start", Call: &call})
	m.Update(watchNoticeMessage{watchEntered(0.9)})
	content, _ := m.transcriptContent(100)
	if content = ansi.Strip(content); !strings.Contains(content, "1 running") || strings.Contains(content, "unfinished") {
		t.Fatalf("tool group not shown as running:\n%s", content)
	}
}
