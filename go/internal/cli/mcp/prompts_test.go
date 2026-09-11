package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func getPromptReq(args map[string]string) mcpgo.GetPromptRequest {
	req := mcpgo.GetPromptRequest{}
	req.Params.Arguments = args
	return req
}

func TestDeployAppPrompt_MentionsRunAndPath(t *testing.T) {
	s := newTestServer(nil, nil)
	res, err := s.handleDeployAppPrompt(context.Background(), getPromptReq(map[string]string{"project_path": "/tmp/myapp"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	tc, ok := res.Messages[0].Content.(mcpgo.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Messages[0].Content)
	}
	if !strings.Contains(tc.Text, "/tmp/myapp") || !strings.Contains(tc.Text, "run") {
		t.Errorf("prompt should reference the project path and the run tool; got: %s", tc.Text)
	}
}

func TestDeployAppPrompt_UsesRealDeviceParamName(t *testing.T) {
	// The run tool's device selector is device_name, not device. The prompt
	// must render the real param name so an agent doesn't pass an invented one.
	s := newTestServer(nil, nil)
	res, err := s.handleDeployAppPrompt(context.Background(), getPromptReq(map[string]string{"device_name": "edge-01"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := res.Messages[0].Content.(mcpgo.TextContent).Text
	if !strings.Contains(text, `device_name="edge-01"`) {
		t.Errorf("prompt should render device_name=; got: %s", text)
	}
	if strings.Contains(text, `, device="edge-01"`) {
		t.Errorf("prompt must not render the invented 'device' param; got: %s", text)
	}
}

func TestDiagnoseContainerPrompt_MentionsDiagnostics(t *testing.T) {
	s := newTestServer(nil, nil)
	res, err := s.handleDiagnoseContainerPrompt(context.Background(), getPromptReq(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc := res.Messages[0].Content.(mcpgo.TextContent)
	if !strings.Contains(tc.Text, "container_list") {
		t.Errorf("diagnose prompt should reference container_list; got: %s", tc.Text)
	}
}
