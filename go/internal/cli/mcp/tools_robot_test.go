package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

func TestRobotLocalizeSchemaPreservesApprovalAndOptionalSelection(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv := server.NewMCPServer("test", "1")
	s.registerRobotTools(srv)
	tool := srv.GetTool("robot_command")
	if tool == nil || boolVal(t, tool.Tool.Annotations.ReadOnlyHint, "ReadOnlyHint") {
		t.Fatal("localization must preserve the robot command approval requirement")
	}
	encoded, err := json.Marshal(tool.Tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, action := range schema.Properties["action"].Enum {
		found[action] = true
	}
	if !found["localize"] || !found["localize_active"] {
		t.Fatal("robot command schema does not distinguish passive and active localization")
	}
	if !found["find_active"] {
		t.Fatal("bounded object search action is missing")
	}
	for _, field := range []string{"query", "model"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Fatalf("object search schema is missing %s", field)
		}
	}
	for _, field := range schema.Required {
		if field == "serial" || field == "destination" {
			t.Fatalf("automatic robot/room selection unexpectedly requires %s", field)
		}
	}
}

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
	if result.Meta == nil || result.Meta.AdditionalFields["robot_serial"] != "robot-a" {
		t.Fatal("missing resolved robot identity on command receipt")
	}
	stop, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{"action": "stop", "serial": "robot-b", "request_id": "stop-once"}))
	if err != nil || stop.IsError || stop.Meta == nil || stop.Meta.AdditionalFields["robot_serial"] != "robot-b" {
		t.Fatal("explicit Stop lost its distinct target", stop, err)
	}
	for _, args := range []map[string]any{
		{"action": "forward", "request_id": "forward-once", "distance_meters": 1.25},
		{"action": "go_to_object", "request_id": "object-once", "evidence_id": "opaque-evidence"},
		{"action": "localize", "request_id": "localize-auto"},
		{"action": "localize", "request_id": "localize-room", "destination": "Living Room"},
		{"action": "localize_active", "request_id": "localize-active", "destination": "Living Room"},
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
	if result.Meta != nil && result.Meta.AdditionalFields["robot_serial"] != nil {
		t.Fatal("ambiguous pairing acquired a resolved identity")
	}
}
