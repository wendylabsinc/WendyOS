package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// A simple imperative search authorizes looking beyond the current camera
// view. Complex, quoted, conditional and explicitly stationary requests remain
// with the model, without automatic movement from this workflow.
func requestedObjectSearch(prompt string) string {
	query, _ := requestedObjectGoal(prompt)
	return query
}

func requestedObjectGoal(prompt string) (string, bool) {
	text := strings.TrimSpace(prompt)
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "please ") {
		text = strings.TrimSpace(text[7:])
		lower = strings.ToLower(text)
	}
	// These complete imperatives include authorization to search for a target
	// outside the current view. The finite object grammar below still rejects
	// conditions, quoted commands and stationary/view-only qualifiers.
	for _, prefix := range []string{"make the go2 ", "make the robot ", "have the go2 ", "have the robot "} {
		if strings.HasPrefix(lower, prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			lower = strings.ToLower(text)
			break
		}
	}
	prefix := ""
	for _, candidate := range []string{"find ", "walk to ", "go to ", "navigate to ", "approach "} {
		if strings.HasPrefix(lower, candidate) {
			prefix = candidate
			break
		}
	}
	if prefix == "" {
		return "", false
	}
	query := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text[len(prefix):]), ".!"))
	if simplePhysicalObject(query) == "" {
		return "", false
	}
	return query, prefix != "find "
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
	normalized := strings.Join(words, " ")
	if len(words) > 0 && (words[0] == "nearest" || words[0] == "closest") {
		words = words[1:]
	}
	if len(words) == 0 || len(words) > 5 {
		return ""
	}
	switch words[len(words)-1] {
	case "chair", "door", "table", "sofa", "couch", "desk", "bin", "box", "bottle", "cup", "mug", "bag", "backpack", "cabinet", "shelf", "plant", "window", "stool", "bench", "book", "keyboard", "monitor", "laptop", "screwdriver", "person", "human":
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
	return normalized
}

// Find/navigation requests authorize bounded search by default. Earlier
// explicit motion restrictions defer to the model, which sees their scope.
// Ordinary chat history must not silently turn off movement prerequisites.
var stationaryInstruction = regexp.MustCompile(`(?i)\b(?:(?:do not|don't|don’t|never|without|no)\s+(?:\w+\s+){0,3}(?:move|moving|motion|movement|walk|walking|turn|turning|rotate|rotation)|(?:stay|remain|remaining|keep|stand)\s+(?:\w+\s+){0,3}(?:still|stationary|here|there|place|where you are)|(?:feet|foot)\s+(?:\w+\s+){0,2}planted|(?:ask|confirm|approval|permission)\s+(?:\w+\s+){0,4}(?:mov\w*|walk\w*|turn\w*))\b`)

func automaticObjectSearchHistory(messages []Message) bool {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		text := strings.TrimSpace(strings.ToLower(message.Content))
		if text == "stop" || text == "freeze" || stationaryInstruction.MatchString(text) {
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
	query             string
	navigate          bool
	serial, model     string
	refreshed         bool
	approachAttempted bool
	queued            *ToolCall
	activeAttempted   bool
	stopped           bool
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
		return nil, errors.New("the next object-search or approach action is unavailable under the current tool policy; no further movement was started")
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
	case "go_to_object":
		f.approachAttempted = true
		f.queued = nil
	}
}

// queue uses only fresh tool evidence. The backend revalidates its opaque
// evidence ID, pose and measured route atomically when approach is dispatched.
func (f *robotFindWorkflow) queue(name string, args map[string]string) {
	encoded, _ := json.Marshal(args)
	f.queued = &ToolCall{Name: name, Arguments: encoded}
}

func (f *robotFindWorkflow) observeResult(call ToolCall, result ToolResult, err error) error {
	if f == nil || f.stopped || f.approachAttempted || err != nil {
		return nil
	}
	var args struct{ Action, Query, Serial, Model string }
	if json.Unmarshal(call.Arguments, &args) != nil {
		return nil
	}
	if call.Name == "robot_command" && args.Action == "find_active" {
		if !f.navigate || !f.activeAttempted || f.refreshed {
			return nil
		}
		var receipt struct {
			State  string
			Search struct {
				Seen  bool
				Query string
			} `json:"object_search"`
		}
		if json.Unmarshal([]byte(result.Text), &receipt) != nil || receipt.State != "completed" || !receipt.Search.Seen {
			return nil
		}
		if f.serial == "" || result.RobotSerial != f.serial || args.Serial != f.serial || args.Model != f.model || simplePhysicalObject(args.Query) != simplePhysicalObject(f.query) || receipt.Search.Query != args.Query {
			return errors.New("the completed search did not match the original robot and target; no approach was started")
		}
		f.refreshed = true
		f.queue("robot_find_object", map[string]string{"query": args.Query, "serial": f.serial, "model": f.model})
		return nil
	}
	if (f.activeAttempted && !f.refreshed) || call.Name != "robot_find_object" || simplePhysicalObject(args.Query) != simplePhysicalObject(f.query) {
		return nil
	}
	var observation struct {
		Seen                 *bool
		Navigable            bool
		Query, Model, Source string
		EvidenceID           string `json:"evidence_id"`
	}
	if json.Unmarshal([]byte(result.Text), &observation) != nil || observation.Seen == nil || (!*observation.Seen && observation.Navigable) {
		return nil
	}
	if *observation.Seen && !f.navigate {
		return nil
	}
	if result.RobotSerial == "" || (args.Serial != "" && args.Serial != result.RobotSerial) || (f.serial != "" && f.serial != result.RobotSerial) {
		return errors.New("the camera result did not identify the same robot; no further movement was started")
	}
	if observation.Query != args.Query || observation.Source != "go2_onboard_camera_local_ollama" || observation.Model == "" || (args.Model != "" && args.Model != observation.Model) || (f.model != "" && f.model != observation.Model) {
		return errors.New("the object lookup did not return matching camera evidence; no further movement was started")
	}
	f.serial, f.model = result.RobotSerial, observation.Model
	if *observation.Seen {
		if observation.Navigable && strings.TrimSpace(observation.EvidenceID) != "" {
			f.queue("robot_command", map[string]string{"action": "go_to_object", "serial": f.serial, "evidence_id": observation.EvidenceID})
		}
		return nil
	}
	if !f.activeAttempted {
		f.queue("robot_command", map[string]string{"action": "find_active", "query": args.Query, "serial": f.serial, "model": f.model})
	}
	return nil
}
