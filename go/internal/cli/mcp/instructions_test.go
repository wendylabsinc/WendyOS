package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// startedProtocolServer runs Start far enough to capture the fully registered
// protocol server, exactly as `wendy mcp serve` builds it.
func startedProtocolServer(t *testing.T) *server.MCPServer {
	t.Helper()
	original := serveStdio
	t.Cleanup(func() { serveStdio = original })
	captured := make(chan *server.MCPServer, 1)
	release := make(chan struct{})
	serveStdio = func(srv *server.MCPServer) error {
		captured <- srv
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- New(&config.Config{}, nil).Start(context.Background()) }()
	select {
	case srv := <-captured:
		t.Cleanup(func() {
			close(release)
			if err := <-done; err != nil {
				t.Errorf("Start: %v", err)
			}
		})
		return srv
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not reach serveStdio")
		return nil
	}
}

func TestInitializeReturnsServerInstructions(t *testing.T) {
	srv := startedProtocolServer(t)
	reply := srv.HandleMessage(context.Background(), json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`))
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Result.Instructions != serverInstructions {
		t.Fatalf("initialize instructions = %q, want serverInstructions", decoded.Result.Instructions)
	}
}

func TestServerInstructionsShape(t *testing.T) {
	text := serverInstructions
	if n := len(text); n < 1200 || n > 2048 {
		t.Fatalf("instructions are %d bytes, want 1200..2048", n)
	}
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			t.Fatalf("non-ASCII byte at %d; keep bytes == characters for client limits", i)
		}
	}
	first, _, _ := strings.Cut(text, "\n\n")
	if len(first) > 512 {
		t.Fatalf("first paragraph is %d bytes; it must be complete within 512", len(first))
	}
	for _, want := range []string{"`wendy_status`", "`run`", "`container_list`", "`telemetry_logs`"} {
		if !strings.Contains(first, want) {
			t.Errorf("first paragraph does not mention %s", want)
		}
	}
	for _, want := range []string{"vm:NAME", "cloud://", "before verifying", "NOT_CONNECTED", "AUTH_REQUIRED", "wendy://guide", "separate words", "Relay CLI update notices", "MCP server restart"} {
		if !strings.Contains(text, want) {
			t.Errorf("instructions do not mention %q", want)
		}
	}
}

// Names the instructions use that are deliberately not tools: run arguments
// (checked against run's schema below) and container_list/run result fields.
var instructionRunArgs = []string{"project_path", "timeout_seconds"}
var instructionResultFields = []string{"running_state", "termination_reason", "not_checked"}

func TestServerInstructionsNameOnlyRegisteredTools(t *testing.T) {
	tools := startedProtocolServer(t).ListTools()
	// Only core tools are advertised before the agent enables a group, and a
	// client cannot call a tool it was never shown.
	registered := func(name string) bool { _, ok := tools[name]; return ok && slices.Contains(toolGroups["core"], name) }

	// Every backticked bare identifier is a tool reference.
	for _, m := range regexp.MustCompile("`([a-z][a-z0-9_]*)`").FindAllStringSubmatch(serverInstructions, -1) {
		if !registered(m[1]) {
			t.Errorf("instructions reference %q, which is not a registered core tool", m[1])
		}
	}
	// Any other snake_case word must be a core tool or a known argument/field.
	for _, word := range regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`).FindAllString(serverInstructions, -1) {
		if !registered(word) && !slices.Contains(instructionRunArgs, word) && !slices.Contains(instructionResultFields, word) {
			t.Errorf("instructions mention %q, which is neither a registered core tool nor an allowlisted argument/field", word)
		}
	}
	run := tools["run"]
	if run == nil {
		t.Fatal("run tool is not registered")
	}
	for _, arg := range slices.Concat(instructionRunArgs, []string{"device"}) {
		if _, ok := run.Tool.InputSchema.Properties[arg]; !ok {
			t.Errorf("instructions describe run argument %q, which run's schema lacks", arg)
		}
	}
}

// The guide is the "read this first" resource the instructions point to; it
// must describe the same run tool.
func TestGuideDescribesConnectedTargetRun(t *testing.T) {
	if strings.Contains(guideText, "to a cloud-enrolled device:\n  run(") {
		t.Fatal("guide still describes run as cloud-only")
	}
	for _, want := range []string{`run(project_path="/path/to/project") // reuse`, "NOT_CONNECTED", "AUTH_REQUIRED", "not_checked"} {
		if !strings.Contains(guideText, want) {
			t.Errorf("guide deploy section is missing %q", want)
		}
	}
}

func TestGuideListsEveryErrorCode(t *testing.T) {
	_, section, _ := strings.Cut(guideText, "## Result shape & error codes")
	section, _, _ = strings.Cut(section, "\n## ")
	for _, code := range allErrorCodes {
		if !strings.Contains(section, string(code)) {
			t.Errorf("guide error-code list is missing %s", code)
		}
	}
}

func TestServerInstructionsSendAuthRequiredToAuthLogin(t *testing.T) {
	if !strings.Contains(serverInstructions, "AUTH_REQUIRED") || !strings.Contains(serverInstructions, "`auth_login`") {
		t.Fatal("instructions must send AUTH_REQUIRED to `auth_login`")
	}
	if !strings.Contains(guideText, "auth_login") {
		t.Fatal("guide must mention auth_login")
	}
}
