package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Saved destinations require a map frame. Check before allocating an identity:
// a rejected controller receipt is immutable and must never be replayed as a
// new movement after localization. Object approaches may use local odometry.
func (t *robotCommandTools) preflightNavigation(ctx context.Context, call ToolCall, args map[string]any, key string, step int, state *robotCommandTurn) (string, error) {
	if args["action"] != "go_to" {
		return "", nil
	}
	t.mu.Lock()
	known := t.pending != nil || state.commands[key][step] != nil || state.connectionChanged
	t.mu.Unlock()
	if known {
		return "", nil
	} // Existing goals retain their original receipt path.
	statusArgs := map[string]any{}
	if serial, ok := args["serial"].(string); ok && serial != "" {
		statusArgs["serial"] = serial
	}
	encoded, _ := json.Marshal(statusArgs)
	statusCall := ToolCall{ID: call.ID, Name: "robot_status", Arguments: encoded}
	available, err := t.base.ListTools(ctx)
	if err != nil {
		return "", err
	}
	allowed := false
	for _, tool := range available {
		if tool.Name == statusCall.Name && validateArguments(tool, encoded) == nil {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errors.New("movement not started: robot_status is unavailable under the current policy; saved-destination alignment cannot be checked")
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := executeControlBase(readCtx, t.base, statusCall)
	if err != nil {
		return "", fmt.Errorf("movement not started: alignment status unavailable: %w", err)
	}
	serial := status.RobotSerial
	if requested, _ := args["serial"].(string); requested != "" {
		if serial != "" && requested != serial {
			return "", errors.New("movement not started: robot status target changed")
		}
		serial = requested
	}
	var result struct{ Snapshot struct{ Aligned *bool } }
	if json.Unmarshal([]byte(status.Text), &result) != nil || result.Snapshot.Aligned == nil {
		return serial, errors.New("movement not started: robot_status did not provide verified alignment state")
	}
	if !*result.Snapshot.Aligned {
		return serial, errors.New("movement not started: saved-destination navigation needs alignment. The movement request permits one bounded localize_active prerequisite unless the user forbids extra motion. Announce it, call robot_command action localize_active, wait for completed and Aligned=true, then retry the original go_to arguments in this turn. No movement receipt was created. Do not proceed after failed, canceled or unknown localization.")
	}
	return serial, nil
}

// Make the prerequisite explicit in status evidence for small local models.
// This states conditional authority; a status question grants no motion.
func navigationPrerequisiteEvidence(result ToolResult) ToolResult {
	var payload map[string]any
	if json.Unmarshal([]byte(result.Text), &payload) != nil {
		return result
	}
	snapshot, _ := payload["snapshot"].(map[string]any)
	aligned, known := snapshot["Aligned"].(bool)
	if !known || aligned {
		return result
	}
	payload["saved_destination_prerequisite"] = map[string]any{
		"action":          "localize_active",
		"required_before": "go_to",
		"condition":       "Only for a requested move when extra motion is allowed. Announce and execute robot_command action localize_active now, within this turn; an announcement alone does not run the prerequisite. Run one bounded attempt, follow its original receipt until completed with Aligned=true, then send the original go_to in this turn. Do not substitute a destination name for a room name; omit localization destination for automatic room selection. No motion for status-only, passive alignment or no-extra-motion requests. Resolve any pending movement receipt first. Failure, cancellation or unknown outcome blocks the move.",
	}
	data, err := json.Marshal(payload)
	if err == nil {
		result.Text = string(data)
	}
	return result
}
