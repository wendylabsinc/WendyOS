package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestObjectSearchFollowsOriginalReceipt(t *testing.T) {
	for _, terminal := range []string{"found", "not-found", "false-completion", "unknown", "transport", "cancel"} {
		t.Run(terminal, func(t *testing.T) {
			ctx, cancel := context.WithCancel(robotTurnContext())
			defer cancel()
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
			base.runResult = func(ctx context.Context, call ToolCall) (ToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(call.Arguments, &args)
				if args["query"] != "the door" || args["model"] != "vision-model" {
					t.Fatal("lost search target", args)
				}
				if len(base.calls) == 1 {
					if args["receipt_only"] == true {
						t.Fatal("initial search was a lookup")
					}
					if terminal == "cancel" {
						cancel()
					}
					return ToolResult{Text: `{"state":"running","message":"Looking","object_search":{"seen":false}}`, RobotSerial: "robot-a"}, nil
				}
				if args["receipt_only"] != true || args["serial"] != "robot-a" || commandIdentity(t, call) != commandIdentity(t, base.calls[0]) {
					t.Fatal("search was restarted or target changed", args)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 20*time.Second {
					t.Fatal("camera lookup lacks its bounded capture window")
				}
				result := ToolResult{RobotSerial: "robot-a"}
				switch terminal {
				case "found":
					result.Text = `{"state":"completed","message":"Object seen; no approach performed","object_search":{"seen":true,"navigable":false}}`
				case "not-found":
					result.Text = `{"state":"failed","message":"Search limit reached","object_search":{"seen":false,"navigable":false}}`
				case "false-completion":
					result.Text = `{"state":"completed","object_search":{"seen":false}}`
				case "unknown":
					result.Text = `{"state":"unknown"}`
				case "transport":
					return result, errors.New("frame transport unavailable")
				}
				return result, nil
			}
			tools := &robotCommandTools{base: base, followInterval: time.Millisecond}
			call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"find_active","query":"the door","model":"vision-model"}`)}
			result, err := tools.ExecuteResult(ctx, call)
			if terminal == "found" || terminal == "not-found" {
				if err != nil || tools.pending != nil || len(base.calls) != 2 || !strings.Contains(result.Text, `"navigable":false`) {
					t.Fatalf("search terminal lost: %s %v", result.Text, err)
				}
			} else if err == nil || tools.pending == nil {
				t.Fatal("unverified search released ownership", err)
			}
			if terminal == "cancel" && len(base.calls) != 1 {
				t.Fatal("canceled search monitor sent another call")
			}
			if terminal == "transport" && len(base.calls) != 4 {
				t.Fatal("unbounded transport retries", len(base.calls))
			}
		})
	}
}

func TestObjectSearchGoalValidationAndIdentity(t *testing.T) {
	for _, raw := range []string{`{}`, `{"query":" "}`, `{"query":"door\n"}`, `{"query":"door","model":42}`, `{"query":"door","model":""}`} {
		var args map[string]any
		_ = json.Unmarshal([]byte(raw), &args)
		args["action"] = "find_active"
		if _, _, _, err := canonicalRobotGoal(args); err == nil {
			t.Fatal("accepted invalid search", raw)
		}
	}
	first := map[string]any{"action": "find_active", "query": "door", "model": "vision-a"}
	_, key, _, err := canonicalRobotGoal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []map[string]any{{"action": "find_active", "query": "chair", "model": "vision-a"}, {"action": "find_active", "query": "door", "model": "vision-b"}} {
		_, next, _, err := canonicalRobotGoal(other)
		if err != nil || next == key {
			t.Fatal("different search reused identity", err)
		}
	}
}

func TestObjectSearchRetryDoesNotStartAnotherSearch(t *testing.T) {
	for _, state := range []string{"failed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
			dispatches := 0
			base.run = func(_ context.Context, call ToolCall) (string, error) {
				var a map[string]any
				_ = json.Unmarshal(call.Arguments, &a)
				if a["receipt_only"] != true {
					dispatches++
				}
				return `{"state":"` + state + `","message":"Original result"}`, nil
			}
			tools := &robotCommandTools{base: base}
			ctx := robotTurnContext()
			call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"find_active","query":"door","model":"vision-a"}`)}
			_, _ = tools.ExecuteResult(ctx, call)
			_, _ = tools.ExecuteResult(ctx, call)
			if dispatches != 1 {
				t.Fatal("retry restarted search", dispatches)
			}
			if state == "unknown" {
				for _, args := range []string{`{"action":"find_active","query":"chair","model":"vision-a"}`, `{"action":"find_active","query":"door","model":"vision-b"}`} {
					_, err := tools.ExecuteResult(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(args)})
					if err == nil || dispatches != 1 {
						t.Fatal("changed search bypassed unknown outcome", err, dispatches)
					}
				}
			}
		})
	}
}
