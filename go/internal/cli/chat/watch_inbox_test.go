package chat

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func watchNotification(method string, fields map[string]any) mcpgo.JSONRPCNotification {
	return mcpgo.JSONRPCNotification{JSONRPC: mcpgo.JSONRPC_VERSION, Notification: mcpgo.Notification{Method: method, Params: mcpgo.NotificationParams{AdditionalFields: fields}}}
}

func TestWatchInboxNeverBlocksAndCountsOverflow(t *testing.T) {
	inbox := newWatchInbox()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < watchInboxSize+3; i++ {
			inbox.handle(watchNotification(watchEventMethod, map[string]any{"watch_id": "w1", "kind": "entered", "sequence": i + 1}))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a full inbox blocked the MCP reader")
	}
	<-inbox.items
	inbox.handle(watchNotification(watchStatusMethod, map[string]any{"watch_id": "w1", "state": "READY"}))
	var last WatchNotice
	for len(inbox.items) > 0 {
		last = <-inbox.items
	}
	if last.State != "READY" || last.Missed != 3 {
		t.Fatalf("the next delivered notice must report the 3 dropped: %+v", last)
	}
	inbox.handle(watchNotification("notifications/other", map[string]any{"watch_id": "w1"}))
	inbox.handle(watchNotification(watchEventMethod, map[string]any{"kind": "entered"})) // no watch_id
	if len(inbox.items) != 0 {
		t.Fatal("non-watch or malformed notifications were delivered")
	}
}

func TestWatchNotificationsReachTheInbox(t *testing.T) {
	t.Setenv("WENDY_CHAT_TEST_MCP", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inbox := newWatchInbox()
	tools, err := newTools(ctx, executable, t.TempDir(), "", inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	if _, err := tools.Execute(ctx, ToolCall{Name: "test_watch_notify", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var got []WatchNotice
	for len(got) < 2 {
		select {
		case notice := <-inbox.items:
			got = append(got, notice)
		case <-ctx.Done():
			t.Fatalf("notifications did not arrive: %+v", got)
		}
	}
	status, event := got[0], got[1]
	if status.Method != watchStatusMethod || status.State != "READY" || status.Camera != "Brio 101" || len(status.Watching) != 1 || status.Watching[0] != "person" {
		t.Fatalf("status %+v", status)
	}
	if event.Method != watchEventMethod || event.Kind != "entered" || event.Sequence != 1 || len(event.Classes) != 1 || event.Classes[0] != (WatchClass{Label: "person", Score: 0.91}) {
		t.Fatalf("event %+v", event)
	}
	select {
	case notice := <-inbox.items:
		t.Fatalf("an unrelated notification was delivered: %+v", notice)
	case <-time.After(100 * time.Millisecond):
	}
}
