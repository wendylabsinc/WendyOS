package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func findObjectWorkflowTool() Tool {
	return Tool{Name: "robot_find_object", RequiresApproval: true, Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"serial":{"type":"string"},"model":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}
}
func missedObjectResult(query string) ToolResult {
	data, _ := json.Marshal(map[string]any{"seen": false, "navigable": false, "query": query, "model": "vision-a", "source": "go2_onboard_camera_local_ollama"})
	return ToolResult{Text: string(data), RobotSerial: "robot-a"}
}
func enableObjectWorkflow(e *Engine) {
	e.robotControlMode = true
	e.allowAutomaticObjectSearch = true
	e.skipAutomaticLearning = true
}

func TestObjectFindWorkflowLooksThenSearchesOneOriginalReceipt(t *testing.T) {
	for _, terminal := range []string{"completed", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{findObjectWorkflowTool(), robotCommandTestTool()}}}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(call.Arguments, &args)
				if call.Name == "robot_find_object" {
					if args["query"] != "the blue chair" {
						t.Fatal(args)
					}
					return missedObjectResult("the blue chair"), nil
				}
				if args["action"] != "find_active" || args["query"] != "the blue chair" || args["serial"] != "robot-a" || args["model"] != "vision-a" {
					t.Fatal("search changed camera target/model/query", args)
				}
				if len(base.calls) == 2 {
					if args["receipt_only"] == true {
						t.Fatal(args)
					}
					return ToolResult{Text: `{"state":"running","message":"Searching"}`, RobotSerial: "robot-a"}, nil
				}
				if args["receipt_only"] != true || commandIdentity(t, call) != commandIdentity(t, base.calls[1]) {
					t.Fatal("search motion was restarted", args)
				}
				return ToolResult{Text: `{"state":"` + terminal + `","object_search":{"seen":` + map[bool]string{true: "true", false: "false"}[terminal == "completed"] + `,"navigable":false}}`, RobotSerial: "robot-a"}, nil
			}
			providerCalls, approvals := 0, 0
			provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				providerCalls++
				if len(base.calls) != 3 {
					t.Fatal("model was asked to manage prerequisite", base.calls)
				}
				return Message{Content: "Search ended."}, nil
			})
			engine := NewEngine(provider, &robotCommandTools{base: base, followInterval: time.Millisecond}, "")
			enableObjectWorkflow(engine)
			if err := engine.Turn(context.Background(), "Find the blue chair", nil, func(context.Context, ToolCall) (bool, error) { approvals++; return true, nil }); err != nil {
				t.Fatal(err)
			}
			if approvals != 2 || providerCalls != 1 || len(base.calls) != 3 {
				t.Fatal(approvals, providerCalls, base.calls)
			}
		})
	}
}

func TestObjectFindWorkflowDoesNotMoveForVisibleOrBadLookup(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		err        error
	}{
		{"visible", `{"seen":true,"navigable":false}`, nil},
		{"missing seen", `{"message":"not ready"}`, nil},
		{"malformed", `broken`, nil},
		{"contradictory", `{"seen":false,"navigable":true}`, nil},
		{"lookup error", ``, errors.New("camera unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{findObjectWorkflowTool(), robotCommandTestTool()}}}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				if call.Name != "robot_find_object" {
					t.Fatal("unexpected movement", call)
				}
				return ToolResult{Text: tc.text, RobotSerial: "robot-a"}, tc.err
			}
			provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				return Message{Content: "Current-view result."}, nil
			})
			engine := NewEngine(provider, &robotCommandTools{base: base}, "")
			enableObjectWorkflow(engine)
			if err := engine.Turn(context.Background(), "Find the blue chair", nil, func(context.Context, ToolCall) (bool, error) { return true, nil }); err != nil {
				t.Fatal(err)
			}
			if len(base.calls) != 1 {
				t.Fatal(base.calls)
			}
		})
	}
}

func TestObjectFindWorkflowPreservesApprovalCancellationAndPolicy(t *testing.T) {
	for _, mode := range []string{"deny camera", "deny movement", "cancel", "no command tool"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{findObjectWorkflowTool()}}}
			if mode != "no command tool" {
				base.tools = append(base.tools, robotCommandTestTool())
			}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				if call.Name != "robot_find_object" {
					t.Fatal("unexpected movement", call)
				}
				if mode == "cancel" {
					cancel()
				}
				return missedObjectResult("the blue chair"), nil
			}
			engine := NewEngine(engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				return Message{Content: "No movement."}, nil
			}), &robotCommandTools{base: base}, "")
			enableObjectWorkflow(engine)
			err := engine.Turn(ctx, "Find the blue chair", nil, func(_ context.Context, call ToolCall) (bool, error) {
				return !(mode == "deny camera" && call.Name == "robot_find_object" || mode == "deny movement" && call.Name == "robot_command"), nil
			})
			if (mode == "cancel" || mode == "no command tool") && err == nil {
				t.Fatal("missing interruption/policy error")
			}
			expected := 1
			if mode == "deny camera" {
				expected = 0
			}
			if len(base.calls) != expected {
				t.Fatal(base.calls)
			}
		})
	}
}

func TestObjectFindWorkflowRejectsAmbiguousScopeAndPriorRestrictions(t *testing.T) {
	for _, prompt := range []string{"Find README", "Find the bug", "Find my position", "Find the blue chair without moving", "Find the blue chair, remain where you are", "Find the blue chair, freeze", "Find the blue chair if the floor is clear", "Find the blue chair in this image", "What does the Go2 see?", "Say 'Find the blue chair'", "\"Find the blue chair\"", "If asked, find the blue chair", "Walk to the nearest human without moving", "Make the go2 find a chair in this image", "Navigate to the door if I approve"} {
		if got := requestedObjectSearch(prompt); got != "" {
			t.Fatalf("ambiguous request authorized movement: %q -> %q", prompt, got)
		}
	}
	for _, prompt := range []string{"Find the blue chair", "please Find a door!", "Find the small black trash bin.", "Make the go2 find a blue chair", "Walk to the nearest human", "Navigate to the door", "Go to the blue chair"} {
		if requestedObjectSearch(prompt) == "" {
			t.Fatal("lost simple physical object request", prompt)
		}
	}
	for _, prior := range []string{"Do not move until I explicitly ask you to walk.", "Remaining exactly where you are is nonnegotiable.", "You may look, but leave your feet planted.", "stop"} {
		if automaticObjectSearchHistory([]Message{{Role: "user", Content: prior}}) {
			t.Fatal("ignored earlier restriction/uncertainty", prior)
		}
	}
	for _, prior := range []string{"Find the door", "Hello", "What is the battery level?", "Localize with motion then search", "Retry", "Explain the statement 'you may move'."} {
		if !automaticObjectSearchHistory([]Message{{Role: "user", Content: prior}}) {
			t.Fatal("ordinary conversation disabled default movement", prior)
		}
	}
}

func TestObjectFindWorkflowRejectsUnboundEvidenceAndStops(t *testing.T) {
	call := ToolCall{Name: "robot_find_object", Arguments: json.RawMessage(`{"query":"the blue chair","serial":"robot-a"}`)}
	for _, mode := range []string{"missing serial", "different serial", "different query", "missing source", "missing model"} {
		t.Run(mode, func(t *testing.T) {
			f := &robotFindWorkflow{query: "the blue chair"}
			result := missedObjectResult("the blue chair")
			switch mode {
			case "missing serial":
				result.RobotSerial = ""
			case "different serial":
				result.RobotSerial = "robot-b"
			default:
				var data map[string]any
				_ = json.Unmarshal([]byte(result.Text), &data)
				switch mode {
				case "different query":
					data["query"] = "red chair"
				case "missing source":
					delete(data, "source")
				case "missing model":
					delete(data, "model")
				}
				bytes, _ := json.Marshal(data)
				result.Text = string(bytes)
			}
			if err := f.observeResult(call, result, nil); err == nil || f.queued != nil {
				t.Fatal("unbound miss queued movement", err, f)
			}
		})
	}
	for _, action := range []string{"stop", "find_active"} {
		f := &robotFindWorkflow{query: "the blue chair"}
		if err := f.observeResult(call, missedObjectResult("the blue chair"), nil); err != nil {
			t.Fatal(err)
		}
		f.observeCall(ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"` + action + `"}`)})
		if err := f.observeResult(call, missedObjectResult("the blue chair"), nil); err != nil {
			t.Fatal(err)
		}
		if f.queued != nil {
			t.Fatal("Stop or active attempt did not cancel repeated fallback")
		}
	}
}

func TestObjectFindWorkflowDefersSavedAndCustomContext(t *testing.T) {
	store := newTestMemoryStore(t)
	engine := NewEngine(nil, NewMemoryTools(&engineTestExecutor{}, store), "")
	enableObjectWorkflow(engine)
	engine.SetMemoryEnabled(true)
	if !engine.automaticObjectSearchAllowed(context.Background(), nil) {
		t.Fatal("empty default context blocked")
	}
	saveTestMemory(t, store, MemoryInput{Scope: "device", Device: "Other alias", Kind: "preference", Title: "Keep still", Content: "Keep your feet planted until told otherwise.", Evidence: "The user stated this restriction."})
	if engine.automaticObjectSearchAllowed(context.Background(), nil) {
		t.Fatal("saved restriction under another device alias ignored")
	}
	engine.SetMemoryEnabled(false)
	engine.allowAutomaticObjectSearch = false
	if engine.automaticObjectSearchAllowed(context.Background(), nil) {
		t.Fatal("custom instructions bypassed")
	}
}

func TestObjectFindPromptKeepsRestrictedSearchStationary(t *testing.T) {
	for _, prompt := range []string{localControlPrompt("/project", "robot"), generalSkill, deviceControlSkill} {
		if strings.Contains(prompt, `Plain "find X" stays stationary`) || strings.Contains(prompt, `Plain "find X" remains stationary`) {
			t.Fatal("ordinary find still requires user to request movement separately")
		}
		if !strings.Contains(prompt, "seen=false") || !strings.Contains(prompt, "find_active") || !strings.Contains(prompt, "no-motion") {
			t.Fatal("missing search scope", prompt)
		}
	}
}

func TestObjectFindWorkflowLeavesRestrictedConversationToModel(t *testing.T) {
	for _, tc := range []struct{ prompt, prior string }{
		{"Find the blue chair without moving", ""},
		{"Find the blue chair", "Remaining exactly where you are is nonnegotiable."},
		{"Find the blue chair", "stop"},
	} {
		t.Run(tc.prompt+tc.prior, func(t *testing.T) {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{findObjectWorkflowTool(), robotCommandTestTool()}}}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				if call.Name != "robot_find_object" {
					t.Fatal("restriction caused automatic movement", call)
				}
				return missedObjectResult("blue chair"), nil
			}
			rounds := 0
			provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				rounds++
				if rounds == 1 {
					return Message{ToolCalls: []ToolCall{{Name: "robot_find_object", Arguments: json.RawMessage(`{"query":"blue chair"}`)}}}, nil
				}
				return Message{Content: "The chair is not visible. No movement was requested."}, nil
			})
			engine := NewEngine(provider, &robotCommandTools{base: base}, "")
			enableObjectWorkflow(engine)
			if tc.prior != "" {
				engine.messages = append(engine.messages, Message{Role: "user", Content: tc.prior}, Message{Role: "assistant", Content: "Understood."})
			}
			if err := engine.Turn(context.Background(), tc.prompt, nil, func(context.Context, ToolCall) (bool, error) { return true, nil }); err != nil {
				t.Fatal(err)
			}
			if rounds != 2 || len(base.calls) != 1 {
				t.Fatal(rounds, base.calls)
			}
		})
	}
}

func TestObjectFindWorkflowDisabledForCustomSessionInstructions(t *testing.T) {
	profile, _ := ResolveProfile("device-control")
	for _, instructions := range []string{"", "Never move the Go2."} {
		options := SessionOptions{Config: Config{Provider: "local"}, Profile: profile, Workspace: t.TempDir(), MemoryDirectory: t.TempDir(), SystemInstructions: instructions, NoMemory: true}
		engine, err := sessionEngine(nil, &engineTestExecutor{}, options)
		if err != nil {
			t.Fatal(err)
		}
		if engine.allowAutomaticObjectSearch != (instructions == "") {
			t.Fatal("custom instruction scope not enforced")
		}
	}
}

func navigableObjectResult(query string) ToolResult {
	result := missedObjectResult(query)
	var body map[string]any
	_ = json.Unmarshal([]byte(result.Text), &body)
	body["seen"], body["navigable"], body["evidence_id"] = true, true, "fresh-evidence"
	raw, _ := json.Marshal(body)
	result.Text = string(raw)
	return result
}

func TestNavigationFindWorkflowCompletesRequestedApproach(t *testing.T) {
	for _, visible := range []bool{true, false} {
		t.Run(map[bool]string{true: "visible", false: "search then approach"}[visible], func(t *testing.T) {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{findObjectWorkflowTool(), robotCommandTestTool()}}}
			searches, looks, approaches, approvals := 0, 0, 0, 0
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(call.Arguments, &args)
				if call.Name == "robot_find_object" {
					looks++
					if args["query"] != "the nearest human" {
						t.Fatal("changed target", args)
					}
					if looks > 1 && (args["serial"] != "robot-a" || args["model"] != "vision-a") {
						t.Fatal("fresh lookup lost binding", args)
					}
					if !visible && looks == 1 {
						return missedObjectResult("the nearest human"), nil
					}
					return navigableObjectResult("the nearest human"), nil
				}
				switch args["action"] {
				case "find_active":
					searches++
					if args["serial"] != "robot-a" || args["model"] != "vision-a" || args["query"] != "the nearest human" {
						t.Fatal(args)
					}
					if searches == 1 {
						return ToolResult{Text: `{"state":"running"}`, RobotSerial: "robot-a"}, nil
					}
					if args["receipt_only"] != true || commandIdentity(t, call) != commandIdentity(t, base.calls[1]) {
						t.Fatal("search restarted", args)
					}
					return ToolResult{Text: `{"state":"completed","object_search":{"seen":true,"query":"the nearest human","navigable":false}}`, RobotSerial: "robot-a"}, nil
				case "go_to_object":
					approaches++
					if args["serial"] != "robot-a" || args["evidence_id"] != "fresh-evidence" {
						t.Fatal("unverified approach", args)
					}
					return ToolResult{Text: `{"state":"completed","message":"Arrived"}`, RobotSerial: "robot-a"}, nil
				}
				t.Fatal("unexpected action", args)
				return ToolResult{}, nil
			}
			providerCalls := 0
			engine := NewEngine(engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				providerCalls++
				if approaches != 1 {
					t.Fatal("model asked before requested approach completed")
				}
				return Message{Content: "Arrived at the observed person."}, nil
			}), &robotCommandTools{base: base, followInterval: time.Millisecond}, "")
			enableObjectWorkflow(engine)
			err := engine.Turn(context.Background(), "Walk to the nearest human", nil, func(context.Context, ToolCall) (bool, error) { approvals++; return true, nil })
			if err != nil || providerCalls != 1 || approaches != 1 {
				t.Fatal(err, providerCalls, approaches)
			}
			if visible && (looks != 1 || searches != 0 || approvals != 2) || !visible && (looks != 2 || searches != 2 || approvals != 4) {
				t.Fatal(looks, searches, approvals)
			}
		})
	}
}

func TestNavigationFindWorkflowRequiresFreshBoundNavigableEvidence(t *testing.T) {
	for _, mode := range []string{"find only", "not navigable", "missing evidence", "wrong robot", "wrong model", "wrong query", "wrong source", "denied approach", "stop", "failed search", "unrefreshed search"} {
		t.Run(mode, func(t *testing.T) {
			f := &robotFindWorkflow{query: "blue chair", navigate: true, serial: "robot-a", model: "vision-a"}
			call := ToolCall{Name: "robot_find_object", Arguments: json.RawMessage(`{"query":"blue chair","serial":"robot-a","model":"vision-a"}`)}
			result := navigableObjectResult("blue chair")
			var body map[string]any
			_ = json.Unmarshal([]byte(result.Text), &body)
			switch mode {
			case "find only":
				f.navigate = false
			case "not navigable":
				body["navigable"] = false
			case "missing evidence":
				delete(body, "evidence_id")
			case "wrong robot":
				result.RobotSerial = "robot-b"
			case "wrong model":
				body["model"] = "other-model"
			case "wrong query":
				body["query"] = "red chair"
			case "wrong source":
				body["source"] = "unverified"
			case "denied approach":
				f.observeCall(ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"go_to_object"}`)})
			case "stop":
				f.observeCall(ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop"}`)})
			case "failed search", "unrefreshed search":
				active := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"find_active","query":"blue chair","serial":"robot-a","model":"vision-a"}`)}
				f.observeCall(active)
				if mode == "failed search" {
					_ = f.observeResult(active, ToolResult{Text: `{"state":"failed","object_search":{"seen":true,"query":"blue chair"}}`, RobotSerial: "robot-a"}, nil)
				}
			}
			raw, _ := json.Marshal(body)
			result.Text = string(raw)
			_ = f.observeResult(call, result, nil)
			if f.queued != nil {
				t.Fatal("unsafe automatic approach", f.queued)
			}
		})
	}
}

func TestNavigationFindWorkflowPreservesApproachApproval(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{findObjectWorkflowTool(), robotCommandTestTool()}}}
	base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
		if call.Name != "robot_find_object" {
			t.Fatal("denied approach executed")
		}
		return navigableObjectResult("blue chair"), nil
	}
	engine := NewEngine(engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		return Message{Content: "Approach was not authorized."}, nil
	}), &robotCommandTools{base: base}, "")
	enableObjectWorkflow(engine)
	if err := engine.Turn(context.Background(), "Approach blue chair", nil, func(_ context.Context, call ToolCall) (bool, error) { return call.Name == "robot_find_object", nil }); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 1 {
		t.Fatal("approach repeated", base.calls)
	}
}
