package chat

import "encoding/json"

// Shape only the model-facing copy. The transcript's raw tool result and the
// controller's cached receipts retain every original field unchanged.
func robotResultEvidence(call ToolCall, text string) (modelText, terminalNotice string) {
	if call.Name != "robot_command" && call.Name != "robot_status" {
		return text, ""
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &body) != nil || body == nil {
		return text, ""
	}
	current, history := splitRobotSnapshotEvidence(body, true)
	outcome, receipt := takeRobotOutcome(body)
	var tracked map[string]json.RawMessage
	if raw, exists := body["pending_command_receipt"]; exists {
		var pending map[string]json.RawMessage
		if json.Unmarshal(raw, &pending) == nil && pending != nil {
			tracked, _ = takeRobotOutcome(pending)
			// A receipt snapshot describes that command, not necessarily the
			// fresh status snapshot returned alongside it.
			_, prior := splitRobotSnapshotEvidence(pending, false)
			for key, value := range prior {
				history["pending_command_receipt."+key] = value
			}
			body["pending_command_receipt"], _ = json.Marshal(pending)
		}
	}
	evidence := struct {
		Outcome map[string]json.RawMessage `json:"command_outcome,omitempty"`
		Tracked map[string]json.RawMessage `json:"tracked_command_outcome,omitempty"`
		Current map[string]json.RawMessage `json:"current_observation_readiness,omitempty"`
		Meaning string                     `json:"evidence_meaning"`
		Other   map[string]json.RawMessage `json:"controller_result"`
		History map[string]json.RawMessage `json:"historical_diagnostics,omitempty"`
	}{outcome, tracked, current,
		"command_outcome state/message is the controller's exact outcome; tracked_command_outcome belongs to the previously tracked command. Current readiness describes observations, not command completion. Historical diagnostics retain older packet faults, prior motion failures and a possibly latched legacy Message; they do not establish this command's cause or current readiness.",
		body, history}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return text, ""
	}
	if call.Name == "robot_command" {
		switch receipt.State {
		case "completed", "failed", "rejected", "canceled", "superseded":
			terminalNotice = receipt.State
			if receipt.Message != "" {
				terminalNotice += ": " + receipt.Message
			}
		}
	}
	return string(encoded), terminalNotice
}

var robotHistoricalFields = []string{"LatestObservationFault", "LastMotionFailure", "Message"}

func splitRobotSnapshotEvidence(body map[string]json.RawMessage, includeCurrent bool) (current, history map[string]json.RawMessage) {
	current, history = make(map[string]json.RawMessage), make(map[string]json.RawMessage)
	var snapshot map[string]json.RawMessage
	if json.Unmarshal(body["snapshot"], &snapshot) != nil || snapshot == nil {
		return current, history
	}
	for _, key := range robotHistoricalFields {
		if value, exists := snapshot[key]; exists {
			history["snapshot."+key] = value
			delete(snapshot, key)
		}
	}
	for _, key := range []string{"ReadinessMessage", "IdleReadinessMessage", "Ready", "MeasurementsReady", "IdleMeasurementsReady", "ScanReady", "PoseReceivedAgeMS", "PoseCaptureAgeMS", "ScanAgeMS"} {
		if !includeCurrent {
			break
		}
		if value, exists := snapshot[key]; exists {
			current[key] = value
			delete(snapshot, key)
		}
	}
	body["snapshot"], _ = json.Marshal(snapshot)
	return current, history
}

func takeRobotOutcome(body map[string]json.RawMessage) (map[string]json.RawMessage, robotReceipt) {
	var receipt robotReceipt
	if json.Unmarshal(body["state"], &receipt.State) != nil {
		return nil, receipt
	}
	switch receipt.State {
	case "running", "completed", "failed", "rejected", "canceled", "superseded", "unknown":
	default:
		return nil, robotReceipt{}
	}
	if raw, exists := body["message"]; exists && json.Unmarshal(raw, &receipt.Message) != nil {
		return nil, robotReceipt{}
	}
	outcome := map[string]json.RawMessage{"state": body["state"]}
	delete(body, "state")
	if raw, exists := body["message"]; exists {
		outcome["message"] = raw
		delete(body, "message")
	}
	return outcome, receipt
}
