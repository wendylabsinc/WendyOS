package chat

import (
	"encoding/json"
	"strings"
)

const localizationContinuationReminder = "No localization tool was executed. Complete the promised localization action only if authorized by the user's current request; otherwise explain the concrete blocker. A navigation request already permits one bounded localize_active prerequisite unless the user forbids extra motion; no separate confirmation is needed. Call robot_command with action localize_active, then continue the requested move only after confirmed alignment. Do not end with a promise or claim execution without a tool result. A status question, passive-only alignment or no-extra-motion request grants no active motion. Do not retry failed, canceled or unknown localization."

// This requests another model decision, never a tool. Its evidence is limited
// to this turn, and any localization/Stop attempt prevents another reminder.
func needsLocalizationContinuation(messages []Message, final string) bool {
	text := strings.ToLower(final)
	if !strings.Contains(text, "align") && !strings.Contains(text, "localiz") && !strings.Contains(text, "localis") {
		return false
	}
	for _, noPromise := range []string{"cannot", "can't", "will not", "won't", "failed", "canceled", "cancelled", "unknown outcome", "blocked"} {
		if strings.Contains(text, noPromise) {
			return false
		}
	}
	promise := false
	for _, marker := range []string{"i will", "i'll", "i’m going to", "i am going to", "executing", "starting", "aligning the robot", "localizing the robot", "localising the robot"} {
		if strings.Contains(text, marker) {
			promise = true
			break
		}
	}
	if !promise {
		return false
	}
	calls := map[string]ToolCall{}
	unaligned := false
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			calls[call.ID] = call
			if call.Name == "robot_command" {
				var args struct{ Action string }
				_ = json.Unmarshal(call.Arguments, &args)
				switch args.Action {
				case "localize", "localize_active", "stop":
					return false
				}
			}
		}
		if message.Role != "tool" || calls[message.ToolCallID].Name != "robot_status" {
			continue
		}
		unaligned = false
		var body struct {
			Snapshot   struct{ Aligned *bool }
			Controller struct{ Snapshot struct{ Aligned *bool } } `json:"controller_result"`
		}
		if json.Unmarshal([]byte(message.Content), &body) != nil {
			continue
		}
		aligned := body.Snapshot.Aligned
		if aligned == nil {
			aligned = body.Controller.Snapshot.Aligned
		}
		unaligned = aligned != nil && !*aligned
	}
	return unaligned
}
