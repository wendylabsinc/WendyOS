package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type robotReceipt struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

type robotMonitoringError struct{ err error }

func (e *robotMonitoringError) Error() string { return e.err.Error() }
func (e *robotMonitoringError) Unwrap() error { return e.err }

func (t *robotCommandTools) ExecuteResult(ctx context.Context, call ToolCall) (result ToolResult, err error) {
	var attempt *robotCommandAttempt
	result, err = t.executeCommandOnce(ctx, call, true, &attempt)
	var args struct{ Action string }
	_ = json.Unmarshal(call.Arguments, &args)
	if err != nil || call.Name != "robot_command" || (args.Action != "localize_active" && args.Action != "find_active") {
		return result, err
	}
	activity := "alignment"
	lookupLimit := 25 * time.Second // One requested camera frame and its submission.
	if args.Action == "find_active" {
		activity = "object search"
	}
	var receipt robotReceipt
	if json.Unmarshal([]byte(result.Text), &receipt) != nil || receipt.State != "running" {
		return result, nil
	}
	if attempt == nil {
		return result, fmt.Errorf("%s monitoring has no pending receipt; completion is unverified", activity)
	}
	defer func() {
		if err != nil {
			err = &robotMonitoringError{err: err}
		}
	}()
	watch, cancel := context.WithDeadline(ctx, attempt.followUntil)
	defer cancel()
	interval := t.followInterval
	if interval <= 0 {
		interval = time.Second
	}
	lastMessage, failures := "", 0
	for {
		if receipt.Message != lastMessage {
			emitRobotProgress(ctx, call, receipt.Message+" Esc cancels monitoring; the robot search continues until Stop or its bounded end.")
			lastMessage = receipt.Message
		}
		timer := time.NewTimer(interval)
		select {
		case <-watch.Done():
			timer.Stop()
			return result, fmt.Errorf("%s monitoring ended; robot completion is unverified and the search may continue until Stop or its five-minute bound: %w", activity, watch.Err())
		case <-timer.C:
		}
		lookupCtx, done := context.WithTimeout(watch, lookupLimit)
		next, lookupErr := t.lookupRobotAttempt(lookupCtx, call, attempt)
		done()
		if lookupErr != nil {
			if watch.Err() != nil {
				return result, fmt.Errorf("%s monitoring canceled; robot stop is not confirmed: %w", activity, watch.Err())
			}
			failures++
			if failures >= 3 || errors.Is(lookupErr, errRobotReceiptUnknown) {
				return result, fmt.Errorf("could not confirm the %s outcome; no movement was restarted: %w", activity, lookupErr)
			}
			emitRobotProgress(ctx, call, "Robot receipt temporarily unavailable; retrying the same receipt without restarting movement.")
			continue
		}
		failures = 0
		result = next
		if json.Unmarshal([]byte(result.Text), &receipt) != nil || receipt.State == "" {
			return result, fmt.Errorf("invalid %s receipt; completion is unverified", activity)
		}
		if receipt.State != "running" {
			return result, nil
		}
	}
}

func emitRobotProgress(ctx context.Context, call ToolCall, message string) {
	if runtime, ok := ctx.Value(turnRuntimeKey{}).(*turnRuntime); ok && runtime.emit != nil {
		runtime.emit(Event{Type: "tool_progress", Text: message, Call: &call})
	}
}

var errRobotReceiptUnknown = errors.New("the controller no longer has this command receipt; the earlier movement outcome remains unknown")

// lookupRobotAttempt can only read the original receipt. It never enters the
// dispatch path, even after a missing receipt, reconnect, timeout or retry.
func (t *robotCommandTools) lookupRobotAttempt(ctx context.Context, call ToolCall, attempt *robotCommandAttempt) (ToolResult, error) {
	t.mu.Lock()
	if attempt.stopped {
		result := attempt.result
		t.mu.Unlock()
		return result, nil
	}
	var args map[string]any
	_ = json.Unmarshal(attempt.arguments, &args)
	args["receipt_only"] = true
	call.Arguments, _ = json.Marshal(args)
	t.mu.Unlock()
	// Recheck the live policy and receipt capability on every read.
	list, err := t.base.ListTools(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	available := false
	for _, tool := range list {
		if tool.Name == "robot_command" && validateArguments(tool, call.Arguments) == nil {
			var schema struct{ Properties map[string]json.RawMessage }
			_ = json.Unmarshal(tool.Parameters, &schema)
			_, available = schema.Properties["receipt_only"]
			break
		}
	}
	if !available {
		return ToolResult{}, errors.New("receipt lookup is unavailable under the current tool policy; no movement was sent")
	}
	result, err := executeControlBase(ctx, t.base, call)
	t.mu.Lock()
	if attempt.stopped {
		result, err = attempt.result, nil
	}
	t.mu.Unlock()
	if targetErr := t.bindRobotTarget(attempt, result.RobotSerial); targetErr != nil {
		return result, targetErr
	}
	if err == nil {
		err = t.recordRobotReceipt(attempt, &result, nil)
	}
	return result, err
}

func (t *robotCommandTools) bindRobotTarget(attempt *robotCommandAttempt, serial string) error {
	if serial == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var args map[string]any
	_ = json.Unmarshal(attempt.arguments, &args)
	explicit, _ := args["serial"].(string)
	if attempt.serial != "" && attempt.serial != serial || explicit != "" && explicit != serial {
		return errors.New("robot receipt target changed; outcome remains unresolved")
	}
	attempt.serial = serial
	args["serial"] = serial
	attempt.arguments, _ = json.Marshal(args)
	return nil
}

func (t *robotCommandTools) recordRobotReceipt(attempt *robotCommandAttempt, result *ToolResult, stoppedAttempt *robotCommandAttempt) error {
	if err := t.bindRobotTarget(attempt, result.RobotSerial); err != nil {
		return err
	}
	var receipt robotReceipt
	if json.Unmarshal([]byte(result.Text), &receipt) != nil || receipt.State == "" {
		return errors.New("invalid robot receipt; the command outcome remains unresolved")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if attempt.stopped {
		*result = attempt.result
		return nil
	}
	if receipt.State == "completed" {
		var args struct{ Action string }
		_ = json.Unmarshal(attempt.arguments, &args)
		if args.Action == "localize_active" {
			var evidence struct{ Snapshot struct{ Aligned bool } }
			_ = json.Unmarshal([]byte(result.Text), &evidence)
			if !evidence.Snapshot.Aligned {
				return errors.New("alignment receipt claims completion without confirmed alignment; outcome remains unresolved")
			}
		}
		if args.Action == "find_active" {
			var evidence struct {
				Search struct {
					Seen bool `json:"seen"`
				} `json:"object_search"`
			}
			_ = json.Unmarshal([]byte(result.Text), &evidence)
			if !evidence.Search.Seen {
				return errors.New("object search claims completion without a visual finding; outcome remains unresolved")
			}
		}
		attempt.completed, attempt.result = true, *result
		if stoppedAttempt != nil && sameRobotTarget(attempt, stoppedAttempt) {
			previous := stoppedAttempt
			previous.stopped, previous.completed = true, false
			previous.result = ToolResult{Text: `{"state":"canceled","message":"A later Stop was acknowledged for this robot. The earlier goal's completion was not established."}`, RobotSerial: result.RobotSerial}
			if t.pending != nil && t.pending.attempt == stoppedAttempt {
				t.pending = nil
			}
		}
	}
	switch receipt.State {
	case "completed", "rejected", "failed", "canceled", "superseded":
		if t.pending != nil && t.pending.attempt == attempt {
			t.pending = nil
		}
	case "unknown":
		return errRobotReceiptUnknown
	case "running":
	default:
		return errors.New("unrecognized robot receipt state; the command outcome remains unresolved")
	}
	return nil
}

// Omitted selection alone is not evidence of an unchanged target. Use trusted
// resolved metadata, or an explicit serial when talking to an older MCP host.
func sameRobotTarget(a, b *robotCommandAttempt) bool {
	serial := func(attempt *robotCommandAttempt) string {
		if attempt.serial != "" {
			return attempt.serial
		}
		var args struct{ Serial string }
		_ = json.Unmarshal(attempt.arguments, &args)
		return args.Serial
	}
	sa, sb := serial(a), serial(b)
	return sa != "" && sa == sb
}

func (t *robotCommandTools) appendPendingReceipt(ctx context.Context, call ToolCall, result ToolResult) (ToolResult, error) {
	t.mu.Lock()
	pending := t.pending
	target := &robotCommandAttempt{arguments: call.Arguments, serial: result.RobotSerial}
	matches := pending != nil && sameRobotTarget(target, pending.attempt)
	t.mu.Unlock()
	if !matches {
		return result, nil
	}
	lookup := call
	lookup.Name = "robot_command"
	receipt, err := t.lookupRobotAttempt(ctx, lookup, pending.attempt)
	var body map[string]any
	if json.Unmarshal([]byte(result.Text), &body) != nil || body == nil {
		return result, nil
	}
	if err != nil {
		body["pending_command_outcome"] = "unverified: " + err.Error()
	} else {
		var evidence any
		if json.Unmarshal([]byte(receipt.Text), &evidence) == nil {
			body["pending_command_receipt"] = evidence
		}
	}
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return result, marshalErr
	}
	result.Text = string(encoded)
	return result, nil
}

func (t *robotCommandTools) pinPendingRobot(call ToolCall) ToolCall {
	var args map[string]any
	if json.Unmarshal(call.Arguments, &args) != nil || args == nil {
		return call
	}
	if serial, _ := args["serial"].(string); serial != "" {
		return call
	}
	t.mu.Lock()
	if t.pending != nil {
		var original struct{ Serial string }
		_ = json.Unmarshal(t.pending.attempt.arguments, &original)
		if original.Serial != "" {
			args["serial"] = original.Serial
		}
	}
	t.mu.Unlock()
	call.Arguments, _ = json.Marshal(args)
	return call
}
