package backend

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// A presentation request must never reach the underlying MCP/media executor.
// Nil device clients similarly fail if any RPC or movement is attempted.
type forbiddenWorldExecutor struct{ t *testing.T }

func (f forbiddenWorldExecutor) ListTools(context.Context) ([]chat.Tool, error) { return nil, nil }
func (f forbiddenWorldExecutor) Execute(context.Context, chat.ToolCall) (string, error) {
	f.t.Fatal("workspace presentation called an executor")
	return "", nil
}
func (f forbiddenWorldExecutor) ExecuteResult(context.Context, chat.ToolCall) (chat.ToolResult, error) {
	f.t.Fatal("workspace presentation called a media executor")
	return chat.ToolResult{}, nil
}

func TestChatWorldWorkspacePresentationContract(t *testing.T) {
	t.Setenv("WENDY_WORLD_PROFILE", "/private/must-not-load.json")
	t.Setenv("WENDY_WORLD_API_KEY", "must-not-read-or-return")
	turn := &ChatTurn{}
	tools := &deviceCardTools{Executor: forbiddenWorldExecutor{t}, conn: &grpcclient.AgentConnection{}, turn: turn}
	listed, err := tools.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var offered *chat.Tool
	for i := range listed {
		if listed[i].Name == showWorldWorkspaceTool {
			offered = &listed[i]
		}
	}
	if offered == nil || offered.RequiresApproval {
		t.Fatal("workspace presentation must be offered without execution approval")
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(offered.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	sections := []string{"operations", "missions", "annotations", "areas", "sessions"}
	if len(schema.Properties) != 1 || !reflect.DeepEqual(schema.Properties["section"].Enum, sections) || !reflect.DeepEqual(schema.Required, []string{"section"}) || schema.AdditionalProperties {
		t.Fatalf("unexpected schema: %s", offered.Parameters)
	}
	for i, section := range sections {
		call := chat.ToolCall{Name: showWorldWorkspaceTool, Arguments: json.RawMessage(`{"section":"` + section + `"}`)}
		result, err := tools.ExecuteResult(context.Background(), call)
		if err != nil {
			t.Fatal(err)
		}
		if turn.Cards[i] != (ChatCard{Kind: "world", Title: "World " + section, Episode: section}) {
			t.Fatalf("unexpected card: %+v", turn.Cards[i])
		}
		for _, required := range []string{"shortcut", "No workspace data was read", "no mission was executed or authority granted", "simulation profile is separate"} {
			if !strings.Contains(result.Text, required) {
				t.Fatalf("missing %q: %s", required, result.Text)
			}
		}
		if strings.Contains(result.Text, "must-not") {
			t.Fatal("environment data disclosed")
		}
	}
}

func TestChatWorldWorkspaceRejectsUntrustedArguments(t *testing.T) {
	for _, args := range []string{
		`{}`, `null`, `[]`, `{"section":null}`, `{"section":1}`, `{"section":""}`, `{"section":"evidence"}`, `{"section":"zones"}`, `{"section":"patrol"}`,
		`{"section":"../missions"}`, `{"section":"missions","endpoint":"https://example.com"}`,
		`{"section":"missions","profile":"/tmp/profile.json"}`, `{"section":"missions","credential":"secret"}`,
		`{"section":"missions","action":"resume"}`, `{"section":"missions","speed":1}`,
		`{"section":"missions"} {}`, `{"section":"missions"} null`, `{"section":"missions"} trailing`,
	} {
		t.Run(args, func(t *testing.T) {
			turn := &ChatTurn{}
			tools := &deviceCardTools{Executor: forbiddenWorldExecutor{t}, turn: turn}
			if _, err := tools.Execute(context.Background(), chat.ToolCall{Name: showWorldWorkspaceTool, Arguments: json.RawMessage(args)}); err == nil {
				t.Fatal("accepted invalid arguments")
			}
			if len(turn.Cards) != 0 {
				t.Fatal("invalid request changed cards")
			}
		})
	}
}

func TestChatWorldWorkspaceCancellationAndCardLimit(t *testing.T) {
	turn := &ChatTurn{}
	tools := &deviceCardTools{Executor: forbiddenWorldExecutor{t}, turn: turn}
	call := chat.ToolCall{Name: showWorldWorkspaceTool, Arguments: json.RawMessage(`{"section":"missions"}`)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tools.Execute(ctx, call); !errors.Is(err, context.Canceled) || len(turn.Cards) != 0 {
		t.Fatalf("cancelled request: %v %+v", err, turn.Cards)
	}
	for i := 0; i < 3; i++ {
		if _, err := tools.Execute(context.Background(), call); err != nil {
			t.Fatal(err)
		}
	}
	if len(turn.Cards) != 1 {
		t.Fatal("duplicate section cards")
	}
	for i := 0; i < 5; i++ {
		turn.Cards = append(turn.Cards, ChatCard{Kind: "camera", Camera: uint32(i)})
	}
	if _, err := tools.Execute(context.Background(), call); err != nil {
		t.Fatal("duplicate at limit should succeed", err)
	}
	call.Arguments = json.RawMessage(`{"section":"operations"}`)
	if _, err := tools.Execute(context.Background(), call); err == nil || len(turn.Cards) != 6 {
		t.Fatalf("card limit not enforced: %v %+v", err, turn.Cards)
	}
	tools.turn = nil
	if _, err := tools.Execute(context.Background(), call); err == nil {
		t.Fatal("missing turn accepted")
	}
}

func TestChatWorldWorkspacePromptRequiresReviewHandoff(t *testing.T) {
	prompt := deviceChatPrompt()
	for _, required := range []string{showWorldWorkspaceTool, "does not read World state or execute missions", "reviews changes in that workspace", "does not grant physical authority", "Use find_with_robot when the user asks"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt missing %q", required)
		}
	}
}

func TestChatWorldWorkspaceEngineOnlyPresentsShortcut(t *testing.T) {
	turn := &ChatTurn{}
	tools := &deviceCardTools{Executor: forbiddenWorldExecutor{t}, turn: turn}
	provider := &deviceToolTestProvider{call: chat.ToolCall{Name: showWorldWorkspaceTool, Arguments: json.RawMessage(`{"section":"missions"}`)}}
	engine := chat.NewEngine(provider, tools, deviceChatPrompt())
	err := engine.Turn(context.Background(), "Show World missions", nil, func(context.Context, chat.ToolCall) (bool, error) {
		t.Fatal("presentation requested execution approval")
		return false, nil
	})
	if err != nil || len(turn.Cards) != 1 || turn.Cards[0].Episode != "missions" {
		t.Fatalf("workspace shortcut not presented: err=%v cards=%+v", err, turn.Cards)
	}
}
