package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRobotRuntimeRetryOnlyLooksUpReceipt(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["action"] != "receipt" || body["request_id"] != "runtime-command-1" || body["serial"] != "robot-a" {
			t.Errorf("retry would replay movement: %v", body)
		}
		fmt.Fprint(w, `{"state":"unknown","message":"Receipt missing"}`)
	}))
	defer endpoint.Close()
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	s.SetRobotDialer(func(ctx context.Context, _ *grpcclient.AgentConnection) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	result, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{"serial": "robot-a", "action": "turn", "degrees": 45, "request_id": "runtime-command-1", "receipt_only": true}))
	if err != nil || result.IsError || structuredMap(t, result)["state"] != "unknown" {
		t.Fatalf("%v %v", result, err)
	}
}

func TestRobotRuntimeDistinguishesRejectedFromUnknownDispatch(t *testing.T) {
	for _, code := range []int{400, 404, 500, 200} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, "invalid response")
			}))
			defer endpoint.Close()
			s := New(&config.Config{}, nil)
			s.SetConn(&grpcclient.AgentConnection{})
			s.SetRobotDialer(func(ctx context.Context, _ *grpcclient.AgentConnection) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
			})
			result, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{"serial": "robot-a", "action": "turn", "degrees": 45, "request_id": "runtime-command-1"}))
			if err != nil || !result.IsError {
				t.Fatalf("%v %v", result, err)
			}
			known, _ := structuredMap(t, result)["command_not_started"].(bool)
			if known != (code == 400 || code == 404) {
				t.Fatalf("status %d marked not started = %v", code, known)
			}
		})
	}
}
