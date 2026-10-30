package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

type testDesktopChatTools struct{ calls atomic.Int32 }

func (*testDesktopChatTools) Close() error { return nil }
func (*testDesktopChatTools) ListTools(context.Context) ([]chat.Tool, error) {
	return []chat.Tool{
		{Name: "container_list", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "container_stop", Parameters: json.RawMessage(`{"type":"object","properties":{"app_name":{"type":"string"}},"required":["app_name"],"additionalProperties":false}`), RequiresApproval: true},
	}, nil
}
func (e *testDesktopChatTools) Execute(_ context.Context, call chat.ToolCall) (string, error) {
	e.calls.Add(1)
	return `{"ok":true}`, nil
}
func testChatTools(context.Context, string) (desktopChatTools, error) {
	return &testDesktopChatTools{}, nil
}

type deviceToolTestProvider struct {
	call   chat.ToolCall
	rounds int
}

func (p *deviceToolTestProvider) Stream(_ context.Context, messages []chat.Message, tools []chat.Tool, emit func(string)) (chat.Message, error) {
	p.rounds++
	if p.rounds == 1 {
		return chat.Message{ToolCalls: []chat.ToolCall{p.call}}, nil
	}
	return chat.Message{Content: messages[len(messages)-1].Content}, nil
}
func TestDesktopChatReadToolsAndApproval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		call  chat.ToolCall
		allow bool
		want  int32
	}{
		{"read", chat.ToolCall{Name: "container_list", Arguments: json.RawMessage(`{}`)}, false, 1},
		{"approved mutation", chat.ToolCall{Name: "container_stop", Arguments: json.RawMessage(`{"app_name":"demo"}`)}, true, 1},
		{"denied mutation", chat.ToolCall{Name: "container_stop", Arguments: json.RawMessage(`{"app_name":"demo"}`)}, false, 0},
		{"invalid arguments", chat.ToolCall{Name: "container_stop", Arguments: json.RawMessage(`{}`)}, true, 0},
		{"unknown tool", chat.ToolCall{Name: "invented", Arguments: json.RawMessage(`{}`)}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &testDesktopChatTools{}
			engine := chat.NewEngine(&deviceToolTestProvider{call: tc.call}, executor, deviceChatPrompt())
			err := engine.Turn(context.Background(), "inspect", nil, func(context.Context, chat.ToolCall) (bool, error) { return tc.allow, nil })
			if err != nil {
				t.Fatal(err)
			}
			if executor.calls.Load() != tc.want {
				t.Fatalf("calls=%d want=%d", executor.calls.Load(), tc.want)
			}
		})
	}
}
func TestDesktopChatApprovalBoundToCallTurnAndSession(t *testing.T) {
	b := webBackend(t)
	ctx, cancel := context.WithCancel(b.life)
	defer cancel()
	state := &ChatTurn{ID: 20, cancel: cancel}
	b.chatTurn = state
	done := make(chan bool, 1)
	go func() {
		ok, _ := state.approve(ctx, chat.ToolCall{Name: "container_stop", Arguments: json.RawMessage(`{"app_name":"demo"}`)})
		done <- ok
	}()
	var pending ChatSnapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pending, _ = b.PollChat(3, 20)
		if pending.ApprovalID != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pending.ApprovalAction != "container_stop" || !strings.Contains(pending.ApprovalApp, "demo") {
		t.Fatalf("bad card: %+v", pending)
	}
	for _, v := range [][3]uint64{{2, 20, pending.ApprovalID}, {3, 21, pending.ApprovalID}, {3, 20, pending.ApprovalID + 1}} {
		if b.ResolveChatApproval(v[0], v[1], v[2], true) == nil {
			t.Fatal("accepted stale approval")
		}
	}
	if err := b.ResolveChatApproval(3, 20, pending.ApprovalID, true); err != nil {
		t.Fatal(err)
	}
	if err := b.ResolveChatApproval(3, 20, pending.ApprovalID, true); err == nil {
		t.Fatal("accepted duplicate approval")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("not approved")
		}
	case <-time.After(time.Second):
		t.Fatal("approval hung")
	}
}
func TestDesktopChatCanceledApprovalCannotExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := &ChatTurn{}
	allowed, err := state.approve(ctx, chat.ToolCall{Name: "container_delete", Arguments: json.RawMessage(`{"app_name":"demo"}`)})
	if allowed || !errors.Is(err, context.Canceled) {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
}
func TestDesktopChatLoadsCoreAndFullSkills(t *testing.T) {
	if !strings.Contains(deviceChatPrompt(), "# WendyOS") {
		t.Fatal("core skill missing")
	}
	profile, _ := chat.ResolveProfile("general")
	tools := &chat.ProfileTools{Base: &testDesktopChatTools{}, Profile: profile}
	result, err := tools.Execute(context.Background(), chat.ToolCall{Name: "profile_skill", Arguments: json.RawMessage(`{"path":"wendy-data/SKILL.md"}`)})
	if err != nil || !strings.Contains(result, "campaign") {
		t.Fatalf("skill=%q err=%v", result, err)
	}
	if _, err := tools.Execute(context.Background(), chat.ToolCall{Name: "profile_skill", Arguments: json.RawMessage(`{"path":"../../config.json"}`)}); err == nil {
		t.Fatal("accepted traversal")
	}
}
