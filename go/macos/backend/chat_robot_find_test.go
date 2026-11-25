package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type chatRobotCatalog struct {
	pbv2.WendyUnitreeServiceClient
	connected bool
}

func (c chatRobotCatalog) ListUnitreePairings(context.Context, *pbv2.ListUnitreePairingsRequest, ...grpc.CallOption) (*pbv2.ListUnitreePairingsResponse, error) {
	return &pbv2.ListUnitreePairingsResponse{Robots: []*pbv2.UnitreeRobot{{Serial: "B42D2000XXXXXXXX", Paired: true, Connected: c.connected}}}, nil
}

type chatNoTools struct{}

func (chatNoTools) ListTools(context.Context) ([]chat.Tool, error) { return nil, nil }
func (chatNoTools) Execute(context.Context, chat.ToolCall) (string, error) {
	return "", nil
}

func TestChatRobotFindNeedsApprovalAConnectedGo2AndNeverClaimsSuccess(t *testing.T) {
	turn := &ChatTurn{}
	tools := &deviceCardTools{Executor: chatNoTools{}, conn: &grpcclient.AgentConnection{UnitreeService: chatRobotCatalog{connected: true}}, turn: turn}
	listed, err := tools.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	approval := false
	for _, tool := range listed {
		approval = approval || tool.Name == robotFindTool && tool.RequiresApproval
	}
	if !approval {
		t.Fatal("robot motion is offered without approval")
	}
	result, err := tools.Execute(context.Background(), chat.ToolCall{Name: robotFindTool, Arguments: json.RawMessage(`{"object":"  the red   toolbox ","bark":true}`)})
	if err != nil || len(turn.Cards) != 1 || turn.Cards[0] != (ChatCard{Kind: "robot_find", Title: "the red toolbox", Camera: 1}) || !strings.Contains(result, "has not been found yet") {
		t.Fatalf("result=%q err=%v cards=%+v", result, err, turn.Cards)
	}
	// One search per response, a real object name, and no invented parameters.
	for _, arguments := range []string{`{"object":"toolbox"}`, `{"object":" "}`, `{"object":"x","speed":2}`} {
		if _, err = tools.Execute(context.Background(), chat.ToolCall{Name: robotFindTool, Arguments: json.RawMessage(arguments)}); err == nil {
			t.Fatalf("accepted %s", arguments)
		}
	}
	tools = &deviceCardTools{Executor: chatNoTools{}, conn: &grpcclient.AgentConnection{UnitreeService: chatRobotCatalog{}}, turn: &ChatTurn{}}
	if _, err = tools.Execute(context.Background(), chat.ToolCall{Name: robotFindTool, Arguments: json.RawMessage(`{"object":"toolbox"}`)}); err == nil || len(tools.turn.Cards) != 0 {
		t.Fatal("showed a search without a connected Go2")
	}
}
