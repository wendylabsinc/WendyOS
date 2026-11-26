package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func robotCommandTestTool() Tool {
	return Tool{Name: "robot_command", Description: "Control robot. Reuse request_id on retries; a running receipt is not completion.", RequiresApproval: true, Parameters: json.RawMessage(`{"type":"object","properties":{"request_id":{"type":"string","minLength":8,"maxLength":80},"receipt_only":{"type":"boolean"},"action":{"type":"string","enum":["turn","stop","connect","localize","localize_active","forward","go_to","remember","go_to_object","find_active"]},"degrees":{"type":"number"},"serial":{"type":"string"},"distance_meters":{"type":"number"},"destination":{"type":"string"},"evidence_id":{"type":"string"},"query":{"type":"string"},"model":{"type":"string"}},"required":["action","request_id"],"additionalProperties":false}`)}
}

func robotTurnContext() context.Context {
	return context.WithValue(context.Background(), turnRuntimeKey{}, &turnRuntime{})
}

func commandIdentity(t *testing.T, call ToolCall) string {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	id, _ := args["request_id"].(string)
	if len(id) < 8 || len(id) > 80 || !strings.HasPrefix(id, "chat-") {
		t.Fatalf("invalid runtime identity: %q", id)
	}
	if _, ok := args["step"]; ok {
		t.Fatal("chat step leaked into backend schema")
	}
	if err := validateArguments(robotCommandTestTool(), call.Arguments); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRobotCommandRuntimeIdentityRetryAndIntentionalRepeat(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		commandIdentity(t, call)
		if len(base.calls) == 1 {
			return "", errors.New("network response lost; outcome unknown")
		}
		return `{"state":"completed","message":"Goal completed"}`, nil
	}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	call := ToolCall{Name: "robot_command", ID: "model-call-1", Arguments: json.RawMessage(`{"action":"turn","degrees":90}`)}
	if _, err := tools.Execute(ctx, call); err == nil {
		t.Fatal("lost transport response did not propagate")
	}
	if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90,"step":2}`)}); err == nil {
		t.Fatal("unknown motion permitted a new step")
	}
	call.ID = "model-retry-new-call-id"
	call.Arguments = json.RawMessage(`{"degrees":90.0,"step":1,"action":"turn"}`)
	if _, err := tools.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if commandIdentity(t, base.calls[0]) != commandIdentity(t, base.calls[1]) {
		t.Fatal("retry changed identity after unknown outcome")
	}
	if strings.Contains(string(base.calls[0].Arguments), "receipt_only") || !strings.Contains(string(base.calls[1].Arguments), `"receipt_only":true`) {
		t.Fatal("retry resubmitted movement instead of looking up its receipt")
	}
	if _, err := tools.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 2 {
		t.Fatal("completed identical step moved again")
	}
	call.Arguments = json.RawMessage(`{"action":"turn","degrees":90,"step":2}`)
	if _, err := tools.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if commandIdentity(t, base.calls[2]) == commandIdentity(t, base.calls[1]) {
		t.Fatal("intentional next step reused previous identity")
	}
	call.Arguments = json.RawMessage(`{"action":"turn","degrees":-90}`)
	if _, err := tools.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if commandIdentity(t, base.calls[3]) == commandIdentity(t, base.calls[0]) {
		t.Fatal("different goal collided")
	}
	call.Arguments = json.RawMessage(`{"action":"turn","degrees":90}`)
	if _, err := tools.Execute(robotTurnContext(), call); err != nil {
		t.Fatal(err)
	}
	if commandIdentity(t, base.calls[len(base.calls)-1]) == commandIdentity(t, base.calls[0]) {
		t.Fatal("new user turn reused identity")
	}
}

func TestRobotCommandRepeatRequiresCompletedReceipt(t *testing.T) {
	for _, outcome := range []string{`{"state":"running"}`, `{"state":"rejected"}`, `{"state":"failed"}`, `{"state":"canceled"}`, `{"state":"superseded"}`, `not a receipt`} {
		t.Run(outcome, func(t *testing.T) {
			base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) { return outcome, nil }}
			tools := &robotCommandTools{base: base}
			ctx := robotTurnContext()
			if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30}`)}); err != nil && outcome != "not a receipt" {
				t.Fatal(err)
			}
			if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30,"step":2}`)}); err == nil {
				t.Fatal("noncompleted receipt permitted repeated motion")
			}
			if len(base.calls) != 1 {
				t.Fatal("blocked repeat reached controller")
			}
		})
	}
}

func TestRobotCommandStopAlwaysReachesController(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) { return `{"state":"completed"}`, nil }}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	for range 2 {
		if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(base.calls) != 2 {
		t.Fatal("cached stop prevented a live stop")
	}
}

func TestRobotCommandUnknownOutcomeSurvivesUserTurnAndCancellation(t *testing.T) {
	for _, firstOutcome := range []string{"network error", "canceled transport", "running receipt", "unparseable result"} {
		t.Run(firstOutcome, func(t *testing.T) {
			base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
			resolve := false
			base.run = func(_ context.Context, call ToolCall) (string, error) {
				if len(base.calls) == 1 {
					switch firstOutcome {
					case "network error":
						return "", errors.New("lost response")
					case "canceled transport":
						return "", context.Canceled
					case "running receipt":
						return `{"state":"running"}`, nil
					default:
						return "tool returned without a receipt", nil
					}
				}
				if strings.Contains(string(call.Arguments), `"receipt_only":true`) && !resolve {
					return `{"state":"running"}`, nil
				}
				return `{"state":"completed"}`, nil
			}
			tools := &robotCommandTools{base: base}
			call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90}`)}
			_, _ = tools.Execute(robotTurnContext(), call)
			nextTurn := robotTurnContext()
			for _, changed := range []string{`{"action":"turn","degrees":45}`, `{"action":"turn","degrees":90,"serial":"new-explicit-serial"}`, `{"action":"turn","degrees":90,"step":2}`} {
				if _, err := tools.Execute(nextTurn, ToolCall{Name: "robot_command", Arguments: json.RawMessage(changed)}); err == nil {
					t.Fatalf("changed goal bypassed pending command: %s", changed)
				}
			}
			// Stop and observation recovery stay usable, but neither operation
			// establishes what happened to the earlier goal's receipt.
			for _, action := range []string{"stop", "connect"} {
				if _, err := tools.Execute(nextTurn, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"` + action + `"}`)}); err != nil {
					t.Fatal(err)
				}
			}
			resolve = true
			if _, err := tools.Execute(nextTurn, call); err != nil {
				t.Fatal(err)
			}
			if len(base.calls) != 6 || commandIdentity(t, base.calls[0]) != commandIdentity(t, base.calls[5]) {
				t.Fatal("later-turn retry changed unresolved identity")
			}
			if _, err := tools.Execute(robotTurnContext(), call); err != nil {
				t.Fatal(err)
			}
			if commandIdentity(t, base.calls[len(base.calls)-1]) == commandIdentity(t, base.calls[0]) {
				t.Fatal("completed goal retained identity on a new user turn")
			}
		})
	}
}

func TestRobotCommandModelSchemaAndPolicyRemainEnforced(t *testing.T) {
	original := robotCommandTestTool()
	base := &engineTestExecutor{tools: []Tool{original}}
	tools := &robotCommandTools{base: base}
	listed := controlToolNames(t, tools)["robot_command"]
	if !listed.RequiresApproval || strings.Contains(string(listed.Parameters), "request_id") || strings.Contains(string(listed.Parameters), "receipt_only") || !strings.Contains(string(listed.Parameters), `"step"`) {
		t.Fatalf("invalid model-facing schema: %+v", listed)
	}
	if string(base.tools[0].Parameters) != string(original.Parameters) {
		t.Fatal("changed backend schema")
	}
	for _, args := range []string{`{"action":"turn","request_id":"model-id"}`, `{"action":"turn","receipt_only":true}`, `{"action":"turn","step":0}`, `{"action":"turn","step":1.5}`, `{"action":"turn","step":65}`, `{"action":"turn","step":2}`} {
		if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(args)}); err == nil {
			t.Fatalf("invalid or out-of-order step accepted: %s", args)
		}
	}
	if _, err := tools.Execute(context.Background(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop"}`)}); err == nil {
		t.Fatal("accepted command outside a turn")
	}
	tools.base = &serviceTools{base: base, allowed: []string{}}
	if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop"}`)}); err == nil {
		t.Fatal("runtime identity bypassed service policy")
	}
	if len(base.calls) != 0 {
		t.Fatal("rejected commands reached backend")
	}
}

func TestRobotCommandIdentityIsHiddenAcrossRealEngineTurns(t *testing.T) {
	profile, _ := ResolveProfile("general")
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) { return `{"state":"completed"}`, nil }}
	round := 0
	provider := engineTestProvider(func(_ context.Context, _ []Message, tools []Tool, _ func(string)) (Message, error) {
		round++
		for _, tool := range tools {
			if tool.Name == "robot_command" && strings.Contains(string(tool.Parameters), "request_id") {
				t.Fatal("provider saw request_id schema")
			}
		}
		if round%2 == 1 {
			return Message{ToolCalls: []ToolCall{{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":15}`)}}}, nil
		}
		return Message{Content: "Completed."}, nil
	})
	engine, err := sessionEngine(provider, base, SessionOptions{Config: Config{Provider: "openai"}, Profile: profile, Workspace: t.TempDir(), MemoryDirectory: t.TempDir(), NoMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	approved := 0
	approve := func(_ context.Context, call ToolCall) (bool, error) {
		approved++
		if strings.Contains(string(call.Arguments), "request_id") {
			t.Fatal("runtime ID exposed for user approval")
		}
		return true, nil
	}
	for range 2 {
		if err := engine.Turn(context.Background(), "Turn left 15 degrees", nil, approve); err != nil {
			t.Fatal(err)
		}
	}
	if approved != 2 || len(base.calls) != 2 || commandIdentity(t, base.calls[0]) == commandIdentity(t, base.calls[1]) {
		t.Fatal("identity not scoped to user turns or approval bypassed")
	}
}

func TestRobotCommandCanonicalGoalAndLocalValidation(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) { return `{"state":"completed"}`, nil }}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	for _, invalid := range []string{`{"action":"turn"}`, `{"action":"turn","degrees":0}`, `{"action":"turn","degrees":181}`, `{"action":"turn","degrees":2.5}`, `{"action":"forward"}`, `{"action":"forward","distance_meters":3}`, `{"action":"go_to","destination":" "}`, `{"action":"go_to_object"}`} {
		if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(invalid)}); err == nil {
			t.Fatalf("invalid movement reached pending state: %s", invalid)
		}
		if tools.pending != nil {
			t.Fatal("invalid movement poisoned pending state")
		}
	}
	for _, args := range []string{`{"action":"go_to","destination":" Kitchen "}`, `{"action":"go_to","destination":"kitchen","degrees":90}`} {
		if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(args)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(base.calls) != 1 {
		t.Fatal("case/whitespace/irrelevant fields duplicated a completed move")
	}
	if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"go_to","destination":"kitchen","serial":"now-explicit"}`)}); err == nil {
		t.Fatal("changed serial duplicated a completed same-turn goal")
	}
}

type commandMetadataExecutor struct {
	*engineTestExecutor
	runResult func(context.Context, ToolCall) (ToolResult, error)
}

func (e *commandMetadataExecutor) ExecuteResult(ctx context.Context, call ToolCall) (ToolResult, error) {
	e.calls = append(e.calls, call)
	return e.runResult(ctx, call)
}

func TestRobotCommandKnownNotStartedAllowsCorrectedSelection(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
	base.runResult = func(context.Context, ToolCall) (ToolResult, error) {
		if len(base.calls) == 1 {
			return ToolResult{Text: "Choose a paired serial", CommandNotStarted: true}, errors.New("pairing selection required")
		}
		return ToolResult{Text: `{"state":"completed"}`}, nil
	}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	_, _ = tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30}`)})
	if tools.pending != nil {
		t.Fatal("known not-started command remained pending")
	}
	if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30,"serial":"selected"}`)}); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 2 || strings.Contains(string(base.calls[1].Arguments), "receipt_only") {
		t.Fatal("known not-started selection could not be corrected")
	}
}

func TestRobotCommandLostReceiptNeverRedispatches(t *testing.T) {
	for _, lookupFailure := range []bool{false, true} {
		base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool(), {Name: "device_connect"}, {Name: "device_disconnect"}, {Name: "cloud_connect"}}}}
		base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
			if len(base.calls) == 1 {
				return ToolResult{}, errors.New("lost initial response")
			}
			if !strings.Contains(string(call.Arguments), `"receipt_only":true`) {
				t.Fatal("unknown command resubmitted movement")
			}
			if lookupFailure {
				return ToolResult{CommandNotStarted: true}, errors.New("lookup not started")
			}
			return ToolResult{Text: `{"state":"unknown","message":"receipt no longer exists"}`}, nil
		}
		tools := &robotCommandTools{base: base}
		call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90}`)}
		_, _ = tools.Execute(robotTurnContext(), call)
		ctx := robotTurnContext()
		for _, name := range []string{"device_connect", "device_disconnect", "cloud_connect"} {
			if _, err := tools.Execute(ctx, ToolCall{Name: name, Arguments: json.RawMessage(`{}`)}); err == nil {
				t.Fatal("connection switched during unresolved motion")
			}
		}
		// A rejected serial change must not poison retrying the original goal.
		_, _ = tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90,"serial":"selected"}`)})
		for range 2 {
			if _, err := tools.Execute(ctx, call); err == nil {
				t.Fatal("missing/failed receipt not reported as error")
			}
			if tools.pending == nil {
				t.Fatal("lookup failure cleared original unresolved movement")
			}
		}
		if len(base.calls) != 4 {
			t.Fatalf("valid receipt queries blocked or invalid calls dispatched: %d", len(base.calls))
		}
		if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":45}`)}); err == nil {
			t.Fatal("new turn bypassed a missing original receipt")
		}
	}
}

func TestRobotCommandConnectionChangeCannotReplayPriorDeviceCompletion(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool(), {Name: "device_connect"}}, run: func(context.Context, ToolCall) (string, error) { return `{"state":"completed"}`, nil }}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30}`)}
	if _, err := tools.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Execute(ctx, ToolCall{Name: "device_connect", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Execute(ctx, call); err == nil {
		t.Fatal("prior device completion was reused after a connection change")
	}
	if _, err := tools.Execute(robotTurnContext(), call); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 3 {
		t.Fatalf("unexpected device calls: %d", len(base.calls))
	}
}

func TestRobotCommandOldRuntimeCannotBlindlyRetryUnknownMotion(t *testing.T) {
	old := robotCommandTestTool()
	var schema map[string]any
	_ = json.Unmarshal(old.Parameters, &schema)
	delete(schema["properties"].(map[string]any), "receipt_only")
	old.Parameters, _ = json.Marshal(schema)
	base := &engineTestExecutor{tools: []Tool{old}, run: func(context.Context, ToolCall) (string, error) { return "", errors.New("lost response") }}
	tools := &robotCommandTools{base: base}
	call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30}`)}
	_, _ = tools.Execute(robotTurnContext(), call)
	if _, err := tools.Execute(robotTurnContext(), call); err == nil {
		t.Fatal("old runtime accepted unsafe retry")
	}
	if len(base.calls) != 1 {
		t.Fatal("old runtime resent ambiguous movement")
	}
}
