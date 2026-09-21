package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/cli/a2a"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/agentservice"
)

func TestChatListsProfilesWithoutModelOrTerminal(t *testing.T) {
	original := jsonOutput
	defer func() { jsonOutput = original }()
	jsonOutput = false
	cmd := newChatCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetArgs([]string{"--list-profiles", "-C", "/does-not-exist"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"general", "developer", "simulation", "debugger", "fleet", "device-reasoning", "device-sensors", "device-control"} {
		if !strings.Contains(out.String(), name) {
			t.Fatal("missing profile", name)
		}
	}
	cmd = newChatCmd()
	cmd.SetArgs([]string{"--profile", "invented"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown chat profile") {
		t.Fatal(err)
	}
}
func TestAgentExampleAndCommands(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"agent", "serve"})
	if err != nil || cmd.Name() != "serve" {
		t.Fatal(cmd, err)
	}
	example := newAgentExampleCmd()
	out := new(bytes.Buffer)
	example.SetOut(out)
	if err := example.Execute(); err != nil {
		t.Fatal(err)
	}
	var c agentservice.Config
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Profile != "device-reasoning" || len(c.Triggers) != 1 || len(c.AllowTools) != 0 {
		t.Fatal(c)
	}
	file := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(file, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := agentservice.LoadConfig(file); err != nil {
		t.Fatal(err)
	}
}
func TestAgentRefusesUnauthenticatedOrCleartextPublicListener(t *testing.T) {
	c := `{"name":"test","profile":"debugger","workspace":".","model":{"provider":"local","model":"test"}}`
	file := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(file, []byte(c), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{{[]string{"--listen", "0.0.0.0:8787"}, "require TLS"}, {nil, "WENDY_AGENT_TOKEN"}, {[]string{"--listen", "localhost:8787"}, "WENDY_AGENT_TOKEN"}} {
		t.Setenv("WENDY_AGENT_TOKEN", "")
		cmd := newAgentServeCmd()
		cmd.SetArgs(append([]string{"--config", file}, test.args...))
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatal(err)
		}
	}
}

func TestAgentTasksFollowsContinuationTokens(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			json.NewEncoder(w).Encode(a2a.TaskList{Tasks: []a2a.Task{{ID: "one"}}, NextPageToken: "next+page", TotalSize: 2})
			return
		}
		if r.URL.Query().Get("pageToken") != "next+page" {
			t.Error("lost continuation token")
		}
		json.NewEncoder(w).Encode(a2a.TaskList{Tasks: []a2a.Task{{ID: "two"}}, TotalSize: 2})
	}))
	defer server.Close()
	client, err := a2a.NewClient(server.URL, "test-access-token-123")
	if err != nil {
		t.Fatal(err)
	}
	list, err := listAllAgentTasks(context.Background(), client)
	if err != nil || len(list.Tasks) != 2 || calls != 2 || list.NextPageToken != "" {
		t.Fatalf("list=%+v calls=%d err=%v", list, calls, err)
	}
}

func TestAgentServeAdvertisesAssignedEphemeralPort(t *testing.T) {
	config := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(config, []byte(`{"name":"test","workspace":".","model":{"provider":"local","model":"test"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_AGENT_TOKEN", "test-access-token-123")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd := newAgentServeCmd()
	cmd.SetContext(ctx)
	cmd.SetErr(writer)
	cmd.SetArgs([]string{"--config", config, "--state-dir", t.TempDir(), "--listen", "127.0.0.1:0"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		if scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	select {
	case line := <-lines:
		parts := strings.Split(line, "listening at ")
		if len(parts) != 2 {
			t.Fatalf("unexpected startup: %s", line)
		}
		endpoint := strings.Split(parts[1], ";")[0]
		if strings.HasSuffix(endpoint, ":0") {
			t.Fatal("advertised port zero")
		}
		resp, err := http.Get(endpoint + "/.well-known/agent-card.json")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !strings.Contains(string(data), endpoint) {
			t.Fatalf("wrong card: %s %v", data, err)
		}
	case err := <-done:
		t.Fatalf("service exited before startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timed out")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
