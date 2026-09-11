package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testWorkspaceTools(t *testing.T) *Tools {
	t.Helper()
	tools, err := newWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tools.Close() })
	return tools
}

func executeTestTool(t *testing.T, tools *Tools, name string, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tools.Execute(context.Background(), ToolCall{Name: name, Arguments: raw})
}

func TestWorkspaceReadWriteAndList(t *testing.T) {
	tools := testWorkspaceTools(t)
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeTestTool(t, tools, "workspace_write", map[string]any{"path": "src/main.go", "content": "one\ntwo\nthree\n"}); err != nil {
		t.Fatal(err)
	}
	if output, err := executeTestTool(t, tools, "workspace_list", map[string]any{}); err != nil || output != "src/\n" {
		t.Fatalf("listing = %q, %v", output, err)
	}
	output, err := executeTestTool(t, tools, "workspace_read", map[string]any{"path": "src/main.go", "offset": 2, "limit": 1})
	if err != nil || !strings.HasPrefix(output, "2: two\n") || strings.Contains(output, "three") {
		t.Fatalf("read = %q, %v", output, err)
	}
	if err := os.Chmod(filepath.Join(tools.workspace, "src/main.go"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTestTool(t, tools, "workspace_write", map[string]any{"path": "src/main.go", "content": "replacement"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(tools.workspace, "src/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("permissions changed: %v", info.Mode())
	}
	if matches, err := filepath.Glob(filepath.Join(tools.workspace, "src/.wendy-chat-*")); err != nil || len(matches) != 0 {
		t.Fatalf("left temporary files: %v, %v", matches, err)
	}
	after, err := os.Getwd()
	if err != nil || before != after {
		t.Fatalf("process directory changed: %q -> %q, %v", before, after, err)
	}
}

func TestWorkspaceRejectsTraversalAndSymlinksOutsideRoot(t *testing.T) {
	tools := testWorkspaceTools(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{secret, "../secret.txt", "nested/../../secret.txt"} {
		for _, tool := range []string{"workspace_read", "workspace_write"} {
			args := map[string]any{"path": name}
			if tool == "workspace_write" {
				args["content"] = "changed"
			}
			if _, err := executeTestTool(t, tools, tool, args); err == nil {
				t.Fatalf("%s accepted escaping path %q", tool, name)
			}
		}
	}
	if err := os.Symlink(outside, filepath.Join(tools.workspace, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := executeTestTool(t, tools, "workspace_read", map[string]any{"path": "escape/secret.txt"}); err == nil {
		t.Fatal("read followed symlink outside workspace")
	}
	if _, err := executeTestTool(t, tools, "workspace_write", map[string]any{"path": "escape/new/file.txt", "content": "changed"}); err == nil {
		t.Fatal("write followed symlink outside workspace")
	}
	if _, err := executeTestTool(t, tools, "workspace_list", map[string]any{"path": "escape"}); err == nil {
		t.Fatal("list followed symlink outside workspace")
	}
	if _, err := executeTestTool(t, tools, "workspace_exec", map[string]any{"cwd": "escape", "command": "echo unsafe"}); err == nil {
		t.Fatal("shell started outside workspace")
	}
	content, err := os.ReadFile(secret)
	if err != nil || string(content) != "private" {
		t.Fatalf("outside file changed: %q, %v", content, err)
	}
}

func TestWorkspaceRejectsInvalidArgsAndNonRegularFiles(t *testing.T) {
	tools := testWorkspaceTools(t)
	for _, test := range []struct {
		name string
		args any
	}{
		{"workspace_write", map[string]any{"path": "file"}},
		{"workspace_read", map[string]any{"path": "."}},
		{"workspace_read", map[string]any{"path": "file", "offset": -1}},
		{"workspace_exec", map[string]any{"command": "echo hi", "timeout_seconds": 601}},
		{"workspace_exec", map[string]any{"command": "echo hi", "surprise": "field"}},
		{"unknown_tool", map[string]any{}},
	} {
		if _, err := executeTestTool(t, tools, test.name, test.args); err == nil {
			t.Fatalf("accepted invalid %s %+v", test.name, test.args)
		}
	}
	if err := os.WriteFile(filepath.Join(tools.workspace, "binary"), []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTestTool(t, tools, "workspace_read", map[string]any{"path": "binary"}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary read error = %v", err)
	}
}

func TestWorkspaceExecDirectoryFailureAndOutputLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	tools := testWorkspaceTools(t)
	if err := os.Mkdir(filepath.Join(tools.workspace, "project"), 0755); err != nil {
		t.Fatal(err)
	}
	output, err := executeTestTool(t, tools, "workspace_exec", map[string]any{"cwd": "project", "command": "pwd"})
	if err != nil || strings.TrimSpace(output) != filepath.Join(tools.workspace, "project") {
		t.Fatalf("shell directory = %q, %v", output, err)
	}
	output, err = executeTestTool(t, tools, "workspace_exec", map[string]any{"command": "echo failure >&2; exit 7"})
	if err == nil || !strings.Contains(output, "failure") || !strings.Contains(err.Error(), "7") {
		t.Fatalf("shell error = %q, %v", output, err)
	}
	output, err = executeTestTool(t, tools, "workspace_exec", map[string]any{"command": "head -c 65536 /dev/zero | tr '\\0' x"})
	if err != nil || len(output) > maxToolOutputBytes || !strings.Contains(output, "truncated") {
		t.Fatalf("shell output limit = %d, %v", len(output), err)
	}
}

func TestWorkspaceExecCancellationStopsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	tools := testWorkspaceTools(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := tools.Execute(ctx, ToolCall{Name: "workspace_exec", Arguments: json.RawMessage(`{"command":"(sleep 1; echo leaked > leaked.txt) & echo ready > ready.txt; wait"}`)})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(tools.workspace, "ready.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shell did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("canceled command error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled command did not stop")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(tools.workspace, "leaked.txt")); !os.IsNotExist(err) {
		t.Fatalf("background child survived cancellation: %v", err)
	}
}

func TestWendyDocsListsAndReadsBundledDocumentation(t *testing.T) {
	tools := testWorkspaceTools(t)
	listing, err := executeTestTool(t, tools, "wendy_docs", map[string]any{})
	if err != nil || !strings.Contains(listing, ".md") {
		t.Fatalf("documentation listing = %q, %v", listing, err)
	}
	name := strings.Split(listing, "\n")[0]
	output, err := executeTestTool(t, tools, "wendy_docs", map[string]any{"path": name, "limit": 2})
	if err != nil || !strings.HasPrefix(output, "1: ") {
		t.Fatalf("documentation read = %q, %v", output, err)
	}
	if _, err := executeTestTool(t, tools, "wendy_docs", map[string]any{"path": "../skills/AGENTS.md"}); err == nil {
		t.Fatal("documentation traversal was accepted")
	}
}
