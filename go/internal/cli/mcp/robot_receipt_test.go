package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func largeRobotReceipt(t *testing.T) string {
	t.Helper()
	hazards := make([]map[string]any, 5000)
	for i := range hazards {
		hazards[i] = map[string]any{"X": 1.23456, "Z": -2.34567, "Size": 0.05, "Yaw": 0, "Drop": false}
	}
	data, err := json.Marshal(map[string]any{"state": "running", "message": "Matching room", "snapshot": map[string]any{"Aligned": false, "Hazards": hazards}})
	if err != nil || len(data) <= 256<<10 || len(data) >= maxRobotReceiptBytes {
		t.Fatal("fixture must exceed old HTTP limit but fit transport budget", err)
	}
	return string(data)
}

func TestRobotReceiptHTTPTransport(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bad        bool
	}{
		{"large-hazard-snapshot", largeRobotReceipt(t), false},
		{"truncated", `{"state":"completed","snapshot":`, true},
		{"null", `null`, true},
		{"trailing-json", `{"state":"completed"}{"state":"failed"}`, true},
		{"oversize", `{"message":"` + strings.Repeat("x", maxRobotReceiptBytes) + `"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var command map[string]any
				if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
					t.Error(err)
				}
				wantAction := "localize_active"
				if requests == 2 {
					wantAction = "receipt"
				}
				if command["action"] != wantAction || command["request_id"] != "same-command-1" {
					t.Errorf("unexpected command or replay: %v", command)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer endpoint.Close()
			s := New(&config.Config{}, nil)
			s.SetConn(&grpcclient.AgentConnection{})
			s.SetRobotDialer(func(ctx context.Context, _ *grpcclient.AgentConnection) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
			})
			for _, lookup := range []bool{false, true} {
				result, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{
					"action": "localize_active", "serial": "robot-a", "request_id": "same-command-1", "receipt_only": lookup,
				}))
				if err != nil || result.IsError != tc.bad {
					t.Fatalf("unexpected result: %v %v", result, err)
				}
				fields := structuredMap(t, result)
				if result.Meta.AdditionalFields["robot_serial"] != "robot-a" || fields["command_not_started"] == true {
					t.Fatal("lost dispatched command identity")
				}
				if tc.bad {
					if fields["error_code"] != string(errCodeInternal) || !strings.Contains(fields["message"].(string), "outcome remains unresolved") {
						t.Fatal("invalid receipt misrepresented as argument error or completion", fields)
					}
				} else if fields["state"] != "running" || len(fields["snapshot"].(map[string]any)["Hazards"].([]any)) != 5000 {
					t.Fatal("valid receipt was truncated")
				}
			}
			if requests != 2 {
				t.Fatalf("unexpected automatic requests: %d", requests)
			}
		})
	}
}

func TestRobotReceiptDecoderBoundary(t *testing.T) {
	valid := `{"state":"running"}`
	for _, extra := range []int{0, 1} {
		body := valid + strings.Repeat(" ", maxRobotReceiptBytes-len(valid)+extra)
		_, err := decodeRobotReceipt(strings.NewReader(body))
		if (err != nil) != (extra == 1) {
			t.Fatalf("boundary +%d: %v", extra, err)
		}
	}
}

func TestCameraHandshakePreservesLargeReceipt(t *testing.T) {
	receipt, shot := searchViewFixture(t)
	body := largeRobotReceipt(t)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, body)
	}))
	defer endpoint.Close()
	out, err := provideRobotSearchView(context.Background(), "robot-a", "same-command-1", receipt,
		func(context.Context, string) (string, cameraSnapshot, int64, error) { return "robot-a", shot, 100, nil },
		func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
		})
	if err != nil || out["state"] != "running" {
		t.Fatal("camera response truncated valid receipt", err)
	}
}
