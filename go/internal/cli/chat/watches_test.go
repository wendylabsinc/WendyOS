package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestToolsStopAllWatchesUsesTheServerDirectly(t *testing.T) {
	t.Setenv("WENDY_CHAT_TEST_MCP", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := newTools(ctx, executable, t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	watches, err := tools.listWatches(ctx)
	if err != nil || len(watches) != 2 || watches[0].Label != "front door" || watches[0].CameraName != "Brio 101" || watches[1].State != "ENDED" {
		t.Fatalf("list %+v %v", watches, err)
	}
	stopped, err := tools.stopAllWatches(ctx)
	if err != nil || stopped != 1 {
		t.Fatalf("stopped %d, %v", stopped, err)
	}
	if _, err := os.Stat(filepath.Join(tools.workspace, "stopped-w1")); err != nil {
		t.Fatal("the active watch was not stopped")
	}
	if _, err := os.Stat(filepath.Join(tools.workspace, "stopped-w2")); err == nil {
		t.Fatal("an ended watch was stopped again")
	}
}

func TestWatchToolsAreHiddenFromChildrenAndServices(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{{Name: "watch_start", RequiresApproval: true}, {Name: "watch_list"}, {Name: "camera_list"}}}
	filtered := withoutWatchTools{base: base}
	list, err := filtered.ListTools(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != "camera_list" {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := filtered.ExecuteResult(context.Background(), ToolCall{Name: "watch_list", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a hidden watch tool ran")
	}
	if _, err := filtered.ExecuteResult(context.Background(), ToolCall{Name: "camera_list", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	profile, err := ResolveProfile("general")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := sessionEngine(engineTestProvider(nil), base, SessionOptions{Profile: profile, Workspace: t.TempDir(), MemoryDirectory: t.TempDir(), ApprovedTools: []string{"watch_start"}})
	if err != nil {
		t.Fatal(err)
	}
	available, err := engine.executor.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range available {
		if isWatchTool(tool.Name) {
			t.Fatalf("an unattended service was offered %s", tool.Name)
		}
	}
}
