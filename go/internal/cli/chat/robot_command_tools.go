package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
)

// robotCommandTools owns transport identities rather than asking a model to
// manufacture them. It wraps an already policy-filtered executor.
type robotCommandTools struct {
	base           Executor
	mu             sync.Mutex
	followInterval time.Duration
	followLimit    time.Duration
	// Retain unknown/running outcomes across user turns. Changing the goal
	// must not accidentally bypass deduplication after a transport failure.
	pending *pendingRobotCommand
}

type pendingRobotCommand struct {
	key     string
	step    int
	turn    *turnRuntime
	attempt *robotCommandAttempt
}

type robotCommandAttempt struct {
	id          string
	arguments   json.RawMessage
	sent        bool
	completed   bool
	stopped     bool
	serial      string
	followUntil time.Time
	result      ToolResult
}

type robotCommandTurn struct {
	connectionChanged bool
	prefix            string
	next              uint64
	commands          map[string]map[int]*robotCommandAttempt
	selections        map[string]string
	selectionOwners   map[string]*robotCommandAttempt
}

func (t *robotCommandTools) ListTools(ctx context.Context) ([]Tool, error) {
	list, err := t.base.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := append([]Tool(nil), list...)
	for i, tool := range out {
		if tool.Name != "robot_command" {
			continue
		}
		out[i], err = robotCommandModelTool(tool)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func robotCommandModelTool(tool Tool) (Tool, error) {
	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
		return Tool{}, fmt.Errorf("reading robot command schema: %w", err)
	}
	if schema == nil {
		return Tool{}, errors.New("missing robot command schema")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		props = make(map[string]any)
		schema["properties"] = props
	}
	delete(props, "request_id")
	delete(props, "receipt_only")
	props["step"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 64, "description": "Omit for the first goal. Retrying the same arguments and step reuses the original goal. To intentionally repeat an identical completed move in this user turn, increment step by one. Never increment after an unknown, running or failed result."}
	if required, ok := schema["required"].([]any); ok {
		filtered := make([]any, 0, len(required))
		for _, field := range required {
			if field != "request_id" && field != "receipt_only" {
				filtered = append(filtered, field)
			}
		}
		schema["required"] = filtered
	}
	// Even older MCP schemas that omit additionalProperties cannot accept
	// model-supplied transport identities through the chat presentation.
	schema["additionalProperties"] = false
	parameters, err := json.Marshal(schema)
	if err != nil {
		return Tool{}, err
	}
	tool.Parameters = parameters
	tool.Description = strings.ReplaceAll(tool.Description, "Reuse request_id on retries; ", "")
	tool.Description += " Chat generates request identities internally; never supply request_id. Repeat identical arguments to retry an uncertain result. Use step only for an intentional repeat after completion."
	return tool, nil
}

func (t *robotCommandTools) Execute(ctx context.Context, call ToolCall) (string, error) {
	result, err := t.ExecuteResult(ctx, call)
	return result.Text, err
}

func (t *robotCommandTools) executeCommandOnce(ctx context.Context, call ToolCall, reconcile bool, selected **robotCommandAttempt) (ToolResult, error) {
	if call.Name != "robot_command" {
		if call.Name == "device_connect" || call.Name == "device_disconnect" || call.Name == "cloud_connect" {
			t.mu.Lock()
			unresolved := t.pending != nil
			if !unresolved {
				if runtime, ok := ctx.Value(turnRuntimeKey{}).(*turnRuntime); ok {
					if value, exists := runtime.robotCommands.Load(t); exists {
						state := value.(*robotCommandTurn)
						if len(state.commands) > 0 {
							state.connectionChanged = true
						}
					}
				}
			}
			t.mu.Unlock()
			if unresolved {
				return ToolResult{}, errors.New("cannot switch the device connection while a robot command has an unresolved outcome; resolve the prior command on this connection first")
			}
		}
		if call.Name == "robot_status" {
			call = t.pinPendingRobot(call)
		}
		result, err := executeControlBase(ctx, t.base, call)
		if err == nil && call.Name == "robot_status" {
			return t.appendPendingReceipt(ctx, call, result)
		}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	list, err := t.base.ListTools(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	var offered *Tool
	receiptLookup := false
	for i := range list {
		if list[i].Name == call.Name {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			_ = json.Unmarshal(list[i].Parameters, &schema)
			_, receiptLookup = schema.Properties["receipt_only"]
			modelTool, err := robotCommandModelTool(list[i])
			if err != nil {
				return ToolResult{}, err
			}
			offered = &modelTool
			break
		}
	}
	if offered == nil {
		return ToolResult{}, errors.New("robot_command is unavailable under the current session policies")
	}
	if err := validateArguments(*offered, call.Arguments); err != nil {
		return ToolResult{}, err
	}
	runtime, ok := ctx.Value(turnRuntimeKey{}).(*turnRuntime)
	if !ok {
		return ToolResult{}, errors.New("robot commands require an active chat turn")
	}
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	if args["action"] == "stop" {
		call = t.pinPendingRobot(call)
		_ = json.Unmarshal(call.Arguments, &args)
	}
	step := 1
	if value, ok := args["step"].(float64); ok {
		step = int(value)
	}
	delete(args, "step")
	args, key, selectionKey, err := canonicalRobotGoal(args)
	if err != nil {
		return ToolResult{}, err
	}
	stop := args["action"] == "stop"
	// Localization can change the navigation frame. Unlike Stop and reading
	// observations, it must not bypass an unresolved movement receipt.
	recovery := stop || args["action"] == "connect" || args["action"] == "status"
	value, _ := runtime.robotCommands.LoadOrStore(t, &robotCommandTurn{})
	state := value.(*robotCommandTurn)
	t.mu.Lock()
	if state.connectionChanged && !recovery {
		t.mu.Unlock()
		return ToolResult{}, errors.New("the device connection changed after robot commands in this turn; start a new user turn for a goal on the selected device so an old result cannot be mistaken for new movement")
	}
	if state.prefix == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.mu.Unlock()
			return ToolResult{}, fmt.Errorf("creating robot command identity: %w", err)
		}
		state.prefix = "chat-" + hex.EncodeToString(nonce[:])
		state.commands = make(map[string]map[int]*robotCommandAttempt)
		state.selections = make(map[string]string)
		state.selectionOwners = make(map[string]*robotCommandAttempt)
	}
	if !recovery {
		serial, _ := args["serial"].(string)
		if previous, exists := state.selections[selectionKey]; exists && previous != serial {
			t.mu.Unlock()
			return ToolResult{}, errors.New("robot selection changed for an identical goal in this turn; preserve the original serial selection when retrying, or start a separate user turn for a different robot")
		}
	}
	steps := state.commands[key]
	if steps == nil {
		steps = make(map[int]*robotCommandAttempt)
		state.commands[key] = steps
	}
	var attempt *robotCommandAttempt
	if pending := t.pending; pending != nil && !recovery {
		// A new turn defaults to step 1; it still resolves the previous
		// unresolved move before starting another. A changed explicit serial
		// or other argument is deliberately not treated as an equivalent retry.
		sameStep := step == pending.step || (runtime != pending.turn && step == 1)
		if key != pending.key || !sameStep {
			t.mu.Unlock()
			if key == pending.key && !sameStep {
				return ToolResult{}, errors.New("cannot increment step while the original command is unresolved; continue its original receipt first")
			}
			if reconcile && receiptLookup {
				if _, lookupErr := t.lookupRobotAttempt(ctx, call, pending.attempt); lookupErr != nil {
					return ToolResult{}, lookupErr
				}
				t.mu.Lock()
				resolved := t.pending == nil
				t.mu.Unlock()
				if resolved {
					return t.executeCommandOnce(ctx, call, false, selected)
				}
			}
			return ToolResult{}, errors.New("the previous robot command is still running or unresolved; no new goal was sent; stop the robot or continue following its original receipt")
		}
		attempt = pending.attempt
		steps[step] = attempt
	}
	if attempt == nil && step > 1 && !recovery {
		previous := steps[step-1]
		if previous == nil || !previous.completed || previous.stopped {
			t.mu.Unlock()
			return ToolResult{}, errors.New("cannot repeat this move until the previous identical step has a completed receipt; retry its original step to resolve an unknown outcome")
		}
	}
	if attempt == nil {
		attempt = steps[step]
	}
	if attempt == nil {
		state.next++
		attempt = &robotCommandAttempt{id: fmt.Sprintf("%s-%d", state.prefix, state.next)}
		args["request_id"] = attempt.id
		attempt.arguments, err = json.Marshal(args)
		if err != nil {
			t.mu.Unlock()
			return ToolResult{}, err
		}
		steps[step] = attempt
	}
	*selected = attempt
	if (attempt.completed || attempt.stopped) && !recovery {
		result := attempt.result
		t.mu.Unlock()
		return result, nil
	}
	if !recovery {
		serial, _ := args["serial"].(string)
		state.selections[selectionKey] = serial
		if state.selectionOwners[selectionKey] == nil {
			state.selectionOwners[selectionKey] = attempt
		}
	}
	if !recovery {
		t.pending = &pendingRobotCommand{key: key, step: step, turn: runtime, attempt: attempt}
	}
	call.Arguments = append(json.RawMessage(nil), attempt.arguments...)
	lookupOnly := attempt.sent && !recovery
	if lookupOnly {
		if !receiptLookup {
			t.mu.Unlock()
			return ToolResult{}, errors.New("the robot command outcome is unresolved and this MCP runtime cannot look up receipts; update the runtime before resolving it, or stop and inspect the robot")
		}
		var retry map[string]any
		_ = json.Unmarshal(call.Arguments, &retry)
		retry["receipt_only"] = true
		call.Arguments, err = json.Marshal(retry)
		if err != nil {
			t.mu.Unlock()
			return ToolResult{}, err
		}
	}
	var stoppedAttempt *robotCommandAttempt
	if stop && t.pending != nil {
		stoppedAttempt = t.pending.attempt
	}
	attempt.sent = true
	if args["action"] == "localize_active" && attempt.followUntil.IsZero() {
		limit := t.followLimit
		if limit <= 0 {
			// Include setup and final receipt delivery after the controller's
			// five-minute search budget; this never extends robot movement.
			limit = 5*time.Minute + 15*time.Second
		}
		attempt.followUntil = time.Now().Add(limit)
	}
	t.mu.Unlock()
	result, err := executeControlBase(ctx, t.base, call)
	t.mu.Lock()
	if attempt.stopped {
		result, err = attempt.result, nil
	}
	t.mu.Unlock()
	if targetErr := t.bindRobotTarget(attempt, result.RobotSerial); targetErr != nil {
		return result, targetErr
	}
	if err != nil && result.CommandNotStarted && !lookupOnly {
		t.mu.Lock()
		attempt.sent = false
		if state.selectionOwners[selectionKey] == attempt {
			delete(state.selections, selectionKey)
			delete(state.selectionOwners, selectionKey)
		}
		if t.pending != nil && t.pending.attempt == attempt {
			t.pending = nil
		}
		t.mu.Unlock()
	}
	if err == nil {
		err = t.recordRobotReceipt(attempt, &result, stoppedAttempt)
	}
	return result, err
}

// The backend ignores action-irrelevant fields and matches destination names
// without case or surrounding space. Those differences cannot create a fresh
// identity. Keep the first wire arguments on the attempt so retries also match
// the backend's exact receipt request comparison.
func canonicalRobotGoal(input map[string]any) (map[string]any, string, string, error) {
	action, _ := input["action"].(string)
	fields := []string{"action"}
	switch action {
	case "turn":
		degrees, _ := input["degrees"].(float64)
		if degrees == 0 || math.Abs(degrees) > 180 || math.Trunc(degrees) != degrees {
			return nil, "", "", errors.New("turn requires integer degrees from -180 to 180, excluding zero")
		}
		fields = append(fields, "degrees")
	case "forward":
		distance, _ := input["distance_meters"].(float64)
		if distance < .1 || distance > 2 {
			return nil, "", "", errors.New("forward requires distance_meters from 0.1 to 2")
		}
		fields = append(fields, "distance_meters")
	case "go_to", "remember":
		destination, _ := input["destination"].(string)
		name := strings.TrimSpace(destination)
		if name == "" || len(name) > 80 || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, "", "", errors.New("name a destination using 1 to 80 printable characters")
		}
		fields = append(fields, "destination")
	case "localize", "localize_active":
		if destination, exists := input["destination"]; exists {
			name, ok := destination.(string)
			name = strings.TrimSpace(name)
			if !ok || name == "" || len(name) > 80 || strings.ContainsFunc(name, unicode.IsControl) {
				return nil, "", "", errors.New("choose a saved room using 1 to 80 printable characters, or omit destination for automatic selection")
			}
			fields = append(fields, "destination")
		}
	case "go_to_object":
		evidence, _ := input["evidence_id"].(string)
		if evidence == "" {
			return nil, "", "", errors.New("go_to_object requires evidence_id from a navigable object observation")
		}
		fields = append(fields, "evidence_id")
	case "stop", "connect", "status":
	default:
		return nil, "", "", fmt.Errorf("unsupported robot command action %q", action)
	}
	args := make(map[string]any, len(fields)+1)
	for _, field := range fields {
		if value, exists := input[field]; exists {
			args[field] = value
		}
	}
	identity := make(map[string]any, len(args)+1)
	for field, value := range args {
		identity[field] = value
	}
	if destination, ok := identity["destination"].(string); ok {
		identity["destination"] = strings.ToLower(strings.TrimSpace(destination))
	}
	selection, err := json.Marshal(identity)
	if err != nil {
		return nil, "", "", err
	}
	if serial, ok := input["serial"].(string); ok && serial != "" {
		args["serial"], identity["serial"] = serial, serial
	}
	canonical, err := json.Marshal(identity)
	return args, string(canonical), string(selection), err
}

func executeControlBase(ctx context.Context, base Executor, call ToolCall) (ToolResult, error) {
	if media, ok := base.(MediaExecutor); ok {
		return media.ExecuteResult(ctx, call)
	}
	text, err := base.Execute(ctx, call)
	return ToolResult{Text: text}, err
}
