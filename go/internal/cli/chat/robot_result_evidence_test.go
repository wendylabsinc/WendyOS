package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const staleFaultReceipt = `{"state":"failed","message":"No unique room match after the bounded search.","snapshot":{"MeasurementsReady":true,"ReadinessMessage":"Fresh Go2 odometry and LiDAR measurements.","PoseReceivedAgeMS":42,"LatestObservationFault":{"at":"2026-11-26T18:02:10Z","error":"LiDAR needs fresh odometry","poseAgeMS":null},"LastMotionFailure":"Earlier turn blocked","Message":"Earlier operation failed","ScanReceivedUnixNanos":1795717000123456789,"Aligned":false}}`

func TestRobotResultSeparatesOutcomeReadinessAndHistory(t *testing.T) {
	for _, action := range []string{"connect", "localize", "localize_active", "turn", "forward", "stop", "go_to", "remember", "go_to_object"} {
		t.Run(action, func(t *testing.T) {
			got, notice := robotResultEvidence(ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"` + action + `"}`)}, staleFaultReceipt)
			if notice != "failed: No unique room match after the bounded search." {
				t.Fatalf("terminal outcome changed: %q", notice)
			}
			var evidence struct {
				Outcome robotReceipt                                  `json:"command_outcome"`
				Current map[string]json.RawMessage                    `json:"current_observation_readiness"`
				Other   struct{ Snapshot map[string]json.RawMessage } `json:"controller_result"`
				History map[string]json.RawMessage                    `json:"historical_diagnostics"`
			}
			if err := json.Unmarshal([]byte(got), &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Outcome.State != "failed" || evidence.Outcome.Message != "No unique room match after the bounded search." || string(evidence.Current["MeasurementsReady"]) != "true" {
				t.Fatal("outcome or current readiness changed")
			}
			if string(evidence.Other.Snapshot["ScanReceivedUnixNanos"]) != "1795717000123456789" {
				t.Fatal("timestamp precision lost")
			}
			if len(evidence.History) != 3 || evidence.Other.Snapshot["LatestObservationFault"] != nil || evidence.Other.Snapshot["ReadinessMessage"] != nil {
				t.Fatal("history or readiness duplicated in current snapshot")
			}
			var source struct{ Snapshot map[string]json.RawMessage }
			_ = json.Unmarshal([]byte(staleFaultReceipt), &source)
			for _, key := range robotHistoricalFields {
				if string(source.Snapshot[key]) != string(evidence.History["snapshot."+key]) {
					t.Fatalf("diagnostic %s changed", key)
				}
			}
			if strings.Count(got, "LiDAR needs fresh odometry") != 1 || !strings.Contains(got, "do not establish this command's cause") {
				t.Fatal("historical cause not labeled or duplicated")
			}
		})
	}
}

func TestRobotStatusPreservesTrackedReceiptWithoutInventingOutcome(t *testing.T) {
	raw := `{"snapshot":{"Ready":true,"Message":"Legacy","LatestObservationFault":null},"pending_command_receipt":` + staleFaultReceipt + `,"destinations":["Kitchen"]}`
	got, notice := robotResultEvidence(ToolCall{Name: "robot_status"}, raw)
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &evidence); err != nil {
		t.Fatal(err)
	}
	if notice != "" || evidence["command_outcome"] != nil || evidence["tracked_command_outcome"] == nil {
		t.Fatal("status invented a new command outcome")
	}
	var other struct {
		Pending      struct{ Snapshot map[string]json.RawMessage } `json:"pending_command_receipt"`
		Destinations []string
	}
	_ = json.Unmarshal(evidence["controller_result"], &other)
	if string(other.Pending.Snapshot["MeasurementsReady"]) != "true" || other.Pending.Snapshot["LatestObservationFault"] != nil || len(other.Destinations) != 1 {
		t.Fatal("tracked snapshot or destinations lost")
	}
	for _, raw := range []string{`{"snapshot":{"Ready":false}}`, `{"state":"unexpected","message":"unknown format"}`, "Tool error: unavailable"} {
		_, notice := robotResultEvidence(ToolCall{Name: "robot_status"}, raw)
		if notice != "" {
			t.Fatal("status produced terminal notice")
		}
	}
	unchanged, notice := robotResultEvidence(ToolCall{Name: "wendy_status"}, staleFaultReceipt)
	if unchanged != staleFaultReceipt || notice != "" {
		t.Fatal("unrelated tool result changed")
	}
}

func TestRobotTerminalNoticePrecedesModelAndRawToolResultStaysIntact(t *testing.T) {
	for _, state := range []string{"completed", "failed", "rejected", "canceled", "superseded", "running", "unknown"} {
		t.Run(state, func(t *testing.T) {
			raw := strings.Replace(staleFaultReceipt, `"state":"failed"`, `"state":"`+state+`"`, 1)
			base := &engineTestExecutor{tools: []Tool{{Name: "robot_command", Parameters: json.RawMessage(`{"type":"object"}`)}}}
			base.run = func(context.Context, ToolCall) (string, error) { return raw, nil }
			var events []Event
			round := 0
			provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
				round++
				if round == 1 {
					return Message{ToolCalls: []ToolCall{{ID: "command", Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize"}`)}}}, nil
				}
				if !strings.HasPrefix(messages[len(messages)-1].Content, `{"command_outcome":`) {
					t.Fatal("model did not receive outcome first")
				}
				terminal := state != "running" && state != "unknown"
				found := false
				for _, e := range events {
					if e.Type == "robot_outcome" {
						found = true
						if e.Text != state+": No unique room match after the bounded search." {
							t.Fatal("outcome paraphrased")
						}
					}
				}
				if found != terminal {
					t.Fatal("visible terminal outcome missing or running misreported")
				}
				return Message{Content: "Result received."}, nil
			})
			e := NewEngine(provider, base, "")
			if err := e.Turn(context.Background(), "Align without moving", func(event Event) { events = append(events, event) }, nil); err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == "tool_result" && event.Text != raw {
					t.Fatal("raw tool transcript changed")
				}
			}
		})
	}
	m := newChatModel(context.Background(), UIOptions{})
	m.handleEvent(Event{Type: "robot_outcome", Text: "failed: Exact controller reason"})
	last := m.transcript[len(m.transcript)-1]
	if isActivityEntry(last) || last.title != "Robot outcome" || last.text != "failed: Exact controller reason" {
		t.Fatal("terminal notice hidden in collapsed tools")
	}
}

func TestRobotTerminalOutcomeSurvivesLargeSnapshot(t *testing.T) {
	raw := `{"snapshot":{"large_diagnostic":"` + strings.Repeat("x", maxToolOutputBytes) + `"},"state":"failed","message":"No unique match."}`
	base := &engineTestExecutor{tools: []Tool{{Name: "robot_command", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	base.run = func(context.Context, ToolCall) (string, error) { return raw, nil }
	notice := ""
	round := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		round++
		if round == 1 {
			return Message{ToolCalls: []ToolCall{{ID: "command", Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize"}`)}}}, nil
		}
		if notice != "failed: No unique match." || !strings.HasPrefix(messages[len(messages)-1].Content, `{"command_outcome":{"message":"No unique match.","state":"failed"}`) {
			t.Fatal("large snapshot hid the exact terminal outcome")
		}
		return Message{Content: "Result received."}, nil
	})
	e := NewEngine(provider, base, "")
	if err := e.Turn(context.Background(), "Align without moving", func(event Event) {
		if event.Type == "robot_outcome" {
			notice = event.Text
		}
	}, nil); err != nil {
		t.Fatal(err)
	}
}
