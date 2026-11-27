package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestLocalizationCameraSuppliesOnlyRequestedView(t *testing.T) {
	receipt, shot := searchViewFixture(t)
	delete(receipt, "object_search")
	viewID := uuid.NewString()
	receipt["localization_camera"] = map[string]any{"view_id": viewID, "state": "waiting"}
	captures, posts := 0, 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/robot/localize-view" {
			t.Errorf("wrong camera destination: %s %s", r.Method, r.URL.Path)
		}
		var q map[string]any
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		if q["view_id"] != viewID || q["request_id"] != "align-once" || q["serial"] != "robot-a" || q["gateway_received_unix_nanos"] != float64(101) {
			t.Error("lost camera demand identity/freshness")
		}
		// The next frame must use the next receipt poll, not loop indefinitely.
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "running", "localization_camera": map[string]any{"view_id": uuid.NewString()}})
	}))
	defer endpoint.Close()
	_, err := provideRobotCameraView(context.Background(), "robot-a", "align-once", receipt, "localization_camera", "/v1/robot/localize-view",
		func(context.Context, string) (string, cameraSnapshot, int64, error) {
			captures++
			return "robot-a", shot, 100, nil
		},
		func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
		})
	if err != nil || captures != 1 || posts != 1 {
		t.Fatalf("unbounded or failed localization capture: %v captures=%d posts=%d", err, captures, posts)
	}
}

func TestLocalizationCameraFailureKeepsAcknowledgedReceipt(t *testing.T) {
	receipt := map[string]any{"state": "running", "message": "Waiting for camera", "localization_camera": map[string]any{"view_id": uuid.NewString()}}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	defer endpoint.Close()
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{}) // No camera service; localization can still use LiDAR.
	s.SetRobotDialer(func(ctx context.Context, _ *grpcclient.AgentConnection) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	for _, lookup := range []bool{false, true} {
		result, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{"action": "localize_active", "serial": "robot-a", "request_id": "align-once", "receipt_only": lookup}))
		if err != nil || result.IsError {
			t.Fatalf("optional camera failure abandoned localization: %v", err)
		}
		out := structuredMap(t, result)
		if out["state"] != "running" || !strings.Contains(out["message"].(string), "Following the same localization receipt") || out["command_not_started"] == true {
			t.Fatalf("lost actual receipt: %+v", out)
		}
	}
}
