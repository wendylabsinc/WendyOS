package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type pairingListClient struct {
	pb.WendyUnitreeServiceClient
	robots []*pb.UnitreeRobot
}

func (c *pairingListClient) ListUnitreePairings(context.Context, *pb.ListUnitreePairingsRequest, ...grpc.CallOption) (*pb.ListUnitreePairingsResponse, error) {
	return &pb.ListUnitreePairingsResponse{Robots: c.robots}, nil
}

func TestRobotToolsUseSelectedDeviceAndFixedEndpoint(t *testing.T) {
	var captured map[string]any
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/robot/command" {
			t.Errorf("wrong request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "failed", "message": "Obstacle in turning footprint"})
	}))
	defer endpoint.Close()
	conn := &grpcclient.AgentConnection{UnitreeService: &pairingListClient{robots: []*pb.UnitreeRobot{{Serial: "robot-a", Paired: true, Connected: true}}}}
	s := New(&config.Config{}, nil)
	s.SetConn(conn)
	s.SetRobotDialer(func(ctx context.Context, got *grpcclient.AgentConnection) (net.Conn, error) {
		if got != conn {
			t.Error("lost selected target")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	listed, err := s.handleRobotTool(context.Background(), callToolReq("unitree_pairings", nil))
	if err != nil || listed.IsError {
		t.Fatal(err, listed)
	}
	if rows := listPayload(t, listed, "robots"); len(rows) != 1 || rows[0]["serial"] != "robot-a" {
		t.Fatal(rows)
	}
	result, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{"action": "turn", "degrees": 180, "request_id": "turn-once"}))
	if err != nil || result.IsError {
		t.Fatal(err, result)
	}
	if captured["serial"] != "robot-a" || captured["degrees"] != float64(180) || captured["request_id"] != "turn-once" {
		t.Fatal(captured)
	}
	if structuredMap(t, result)["state"] != "failed" {
		t.Fatal("lost failure evidence", result)
	}
	for _, args := range []map[string]any{
		{"action": "forward", "request_id": "forward-once", "distance_meters": 1.25},
		{"action": "go_to_object", "request_id": "object-once", "evidence_id": "opaque-evidence"},
	} {
		result, err = s.handleRobotTool(context.Background(), callToolReq("robot_command", args))
		if err != nil || result.IsError {
			t.Fatal(err, result)
		}
		for key, want := range args {
			if captured[key] != want {
				t.Fatalf("lost %s: got %v want %v", key, captured[key], want)
			}
		}
	}

}

func TestRobotToolsRequireUnambiguousPairing(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{UnitreeService: &pairingListClient{robots: []*pb.UnitreeRobot{{Serial: "a", Paired: true}, {Serial: "b", Paired: true}}}})
	s.SetRobotDialer(func(context.Context, *grpcclient.AgentConnection) (net.Conn, error) {
		t.Fatal("ambiguous robot reached controller")
		return nil, nil
	})
	result, err := s.handleRobotTool(context.Background(), callToolReq("robot_status", nil))
	if err != nil || !result.IsError {
		t.Fatal("accepted ambiguous robot", result, err)
	}
}
