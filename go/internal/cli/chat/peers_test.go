package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/a2a"
)

func TestRemoteToolUsesConfiguredPeerAndBoundsDelegation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/message:send" || r.Header.Get("Authorization") != "Bearer peer-token-at-least-16" {
			t.Error("wrong peer request")
		}
		var req a2a.SendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		depth, err := a2a.RequestDepth(req.Metadata)
		if err != nil || depth != 4 || req.Message.MessageID != "stable-id" || !req.Configuration.ReturnImmediately {
			t.Error("lost task metadata", req, err)
		}
		_ = json.NewEncoder(w).Encode(a2a.SendResponse{Task: &a2a.Task{ID: "remote-task"}})
	}))
	defer server.Close()
	t.Setenv("WENDY_TEST_PEER_TOKEN", "peer-token-at-least-16")
	s := &agentSupervisor{options: SessionOptions{DelegationDepth: 3, Peers: map[string]PeerSpec{"robot": {URL: server.URL, TokenEnv: "WENDY_TEST_PEER_TOKEN"}}}}
	call := ToolCall{Arguments: json.RawMessage(`{"peer":"robot","action":"send","request_id":"stable-id","prompt":"inspect"}`)}
	out, err := s.remote(context.Background(), call)
	if err != nil || !strings.Contains(out, "remote-task") || calls != 1 {
		t.Fatal(out, err, calls)
	}
	s.options.DelegationDepth = 4
	if _, err := s.remote(context.Background(), call); err == nil || calls != 1 {
		t.Fatal("delegation cycle was not bounded")
	}
}

func TestPeerValidationRequiresStrongConfiguredToken(t *testing.T) {
	peers := map[string]PeerSpec{"robot": {URL: "https://robot.example", TokenEnv: "WENDY_TEST_PEER_TOKEN"}}
	for _, token := range []string{"", "short", "long-but-invalid\ntoken"} {
		t.Setenv("WENDY_TEST_PEER_TOKEN", token)
		if err := ValidatePeers(peers); err == nil {
			t.Fatal("accepted invalid peer token")
		}
	}
}
