package chat

import (
	"encoding/json"
	"strings"
)

func isRobotStopPrompt(prompt string) bool {
	return strings.EqualFold(strings.TrimSpace(prompt), "stop")
}

// State questions need a new observation, even when a small local model would
// otherwise answer from an earlier running receipt. Only offered read tools
// are eligible; the injected Stop follows the ordinary approval path.
func (e *Engine) robotEvidenceCall(prompt string, available []Tool, round int) *ToolCall {
	if round != 0 {
		return nil
	}
	text := strings.ToLower(strings.TrimSpace(prompt))
	priorRobot := e.robotControlMode
	for _, message := range e.messages {
		for _, call := range message.ToolCalls {
			if call.Name == "robot_command" || call.Name == "robot_status" {
				priorRobot = true
			}
		}
	}
	if isRobotStopPrompt(prompt) && priorRobot {
		for _, tool := range available {
			if tool.Name == "robot_command" {
				return &ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop"}`)}
			}
		}
		return nil
	}
	robotContext := priorRobot || strings.Contains(text, "robot") || strings.Contains(text, "go2") || strings.Contains(text, "align") || strings.Contains(text, "localiz")
	for _, prefix := range []string{"how do ", "how can ", "how to ", "what is localization", "what is alignment", "can we "} {
		if strings.HasPrefix(text, prefix) {
			return nil
		}
	}
	question := strings.Contains(text, "?") || strings.HasPrefix(text, "is ") || strings.HasPrefix(text, "are ") || strings.HasPrefix(text, "what") || strings.HasPrefix(text, "how") || strings.HasPrefix(text, "have ") || strings.HasPrefix(text, "has ") || strings.HasPrefix(text, "did ")
	status := strings.Contains(text, "not moving") || strings.Contains(text, "status") || strings.Contains(text, "progress") || question && (strings.Contains(text, "align") || strings.Contains(text, "localiz") || strings.Contains(text, "done") || strings.Contains(text, "still") || strings.Contains(text, "moving"))
	if !robotContext || !status {
		return nil
	}
	for _, tool := range available {
		if tool.Name == "robot_status" && !tool.RequiresApproval {
			return &ToolCall{Name: "robot_status", Arguments: json.RawMessage(`{}`)}
		}
	}
	return nil
}
