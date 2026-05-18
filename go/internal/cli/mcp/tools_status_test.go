package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func TestWendyStatus_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if out["connected"] != false {
		t.Errorf("expected connected=false, got %v", out["connected"])
	}
	if out["suggested_next_step"] == "" {
		t.Error("expected non-empty suggested_next_step")
	}
}

func TestWendyStatus_Connected_Direct(t *testing.T) {
	conn, _ := startFakeAgentServer(t, &fakeAgentServer{})
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)
	srv.SetConnType("direct")

	result, err := srv.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if out["connected"] != true {
		t.Errorf("expected connected=true, got %v", out["connected"])
	}
	if out["connection_type"] != "direct" {
		t.Errorf("expected connection_type=direct, got %v", out["connection_type"])
	}
}
