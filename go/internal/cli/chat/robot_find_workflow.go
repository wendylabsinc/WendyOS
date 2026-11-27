package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode"
)

// A simple imperative search authorizes looking beyond the current camera
// view. Complex, quoted, conditional and explicitly stationary requests remain
// with the model, without automatic movement from this workflow.
func requestedObjectSearch(prompt string) string {
	text := strings.TrimSpace(prompt)
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "please ") {
		text = strings.TrimSpace(text[7:])
		lower = strings.ToLower(text)
	}
	if !strings.HasPrefix(lower, "find ") {
		return ""
	}
	query := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text[5:]), ".!"))
	if simplePhysicalObject(query) == "" {
		return ""
	}
	return query
}

// A finite noun/adjective grammar avoids treating source-code lookup, quoted
// instructions, conditions or "remain where you are" as movement authority.
func simplePhysicalObject(query string) string {
	if query == "" || len(query) > 160 || strings.ContainsFunc(query, unicode.IsControl) {
		return ""
	}
	words := strings.Fields(strings.ToLower(query))
	if len(words) > 0 && (words[0] == "the" || words[0] == "a" || words[0] == "an") {
		words = words[1:]
	}
	if len(words) == 0 || len(words) > 5 {
		return ""
	}
	switch words[len(words)-1] {
	case "chair", "door", "table", "sofa", "couch", "desk", "bin", "box", "bottle", "cup", "mug", "bag", "backpack", "cabinet", "shelf", "plant", "window", "stool", "bench", "book", "keyboard", "monitor", "laptop", "screwdriver":
	default:
		return ""
	}
	for _, word := range words[:len(words)-1] {
		switch word {
		case "blue", "red", "green", "yellow", "black", "white", "brown", "gray", "grey", "orange", "purple", "pink", "silver", "gold", "beige", "small", "large", "big", "wooden", "metal", "plastic", "office", "dining", "trash", "rubbish", "garbage", "recycling":
		default:
			return ""
		}
	}
	return strings.Join(words, " ")
}

// Earlier instructions can constrain motion in arbitrary words. Only earlier
// simple object requests are known not to introduce another condition. Leave
// all other histories to the model rather than infer that a restriction ended.
func automaticObjectSearchHistory(messages []Message) bool {
	for _, message := range messages {
		if message.Role == "user" && requestedObjectSearch(message.Content) == "" {
			return false
		}
	}
	return true
}

func (e *Engine) automaticObjectSearchAllowed(ctx context.Context, history []Message) bool {
	if !e.robotControlMode || !e.allowAutomaticObjectSearch || !automaticObjectSearchHistory(history) {
		return false
	}
	if e.MemoryEnabled() {
		// A note under another device alias can still describe this robot. Any
		// saved note defers authorization to the model, without parsing prose.
		if ctx.Err() != nil {
			return false
		}
		root, err := e.memory.store.openDirectory(false)
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err != nil {
			return false
		}
		defer root.Close()
		files, err := listMemoryFiles(root)
		if err != nil {
			return false
		}
		for _, file := range files {
			if file.Type().IsRegular() && isMemoryFilename(file.Name()) {
				return false
			}
		}
	}
	return true
}

type robotFindWorkflow struct {
	query           string
	queued          *ToolCall
	activeAttempted bool
	stopped         bool
}

func (f *robotFindWorkflow) nextCall(available []Tool, round int) (*ToolCall, error) {
	if f == nil || f.stopped {
		return nil, nil
	}
	call := f.queued
	if call == nil && round == 0 {
		args, _ := json.Marshal(map[string]string{"query": f.query})
		call = &ToolCall{Name: "robot_find_object", Arguments: args}
	}
	if call == nil {
		return nil, nil
	}
	for _, tool := range available {
		if tool.Name == call.Name && validateArguments(tool, call.Arguments) == nil {
			f.queued = nil
			return call, nil
		}
	}
	if f.queued != nil {
		f.queued = nil
		return nil, errors.New("the object was not seen in the current view, but bounded search is unavailable under the current tool policy; no search movement was started")
	}
	return nil, nil
}

func (f *robotFindWorkflow) observeCall(call ToolCall) {
	if f == nil || call.Name != "robot_command" {
		return
	}
	var args struct{ Action string }
	_ = json.Unmarshal(call.Arguments, &args)
	switch args.Action {
	case "stop":
		f.stopped = true
		f.queued = nil
	case "find_active":
		f.activeAttempted = true
		f.queued = nil
	}
}

func (f *robotFindWorkflow) observeResult(call ToolCall, result ToolResult, err error) error {
	if f == nil || f.stopped || f.activeAttempted || call.Name != "robot_find_object" || err != nil {
		return nil
	}
	var args struct{ Query, Serial, Model string }
	if json.Unmarshal(call.Arguments, &args) != nil || simplePhysicalObject(args.Query) != simplePhysicalObject(f.query) {
		return nil
	}
	var observation struct {
		Seen                 *bool
		Navigable            bool
		Query, Model, Source string
	}
	if json.Unmarshal([]byte(result.Text), &observation) != nil || observation.Seen == nil || *observation.Seen || observation.Navigable {
		return nil
	}
	if result.RobotSerial == "" || args.Serial != "" && args.Serial != result.RobotSerial {
		return errors.New("the object was not seen, but the camera result did not identify the same robot; no search movement was started")
	}
	if observation.Query != args.Query || observation.Source != "go2_onboard_camera_local_ollama" || observation.Model == "" {
		return errors.New("the object lookup did not return matching camera evidence; no search movement was started")
	}
	active := map[string]string{"action": "find_active", "query": args.Query, "serial": result.RobotSerial, "model": observation.Model}
	encoded, _ := json.Marshal(active)
	f.queued = &ToolCall{Name: "robot_command", Arguments: encoded}
	return nil
}
