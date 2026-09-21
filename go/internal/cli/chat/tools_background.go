package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

var backgroundTools = []Tool{
	{
		Name: "camera_view", RequiresApproval: true,
		Description: "Show the user a live device camera in a window on this computer. Starts the bundled Wendy CLI in the background against the current connected device and returns a job_id promptly; chat remains usable. Uses installed GStreamer; startup errors appear in background_process_list. A running process does not yet prove playback. This is for the user's eyes: use camera_snapshot separately for model visual inspection. Repeated identical requests reuse a running viewer. Stops on background_process_stop or chat exit.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"camera_id":{"type":"integer","minimum":0,"maximum":4294967295,"description":"Camera ID from camera_list; omit to auto-select a single camera"},"stable_id":{"type":"string","minLength":1,"maxLength":255,"description":"Stable camera ID; mutually exclusive with camera_id"},"width":{"type":"integer","minimum":0,"maximum":8192},"height":{"type":"integer","minimum":0,"maximum":8192},"fps":{"type":"integer","minimum":0,"maximum":240}},"additionalProperties":false}`),
	},
	{
		Name: "audio_listen", RequiresApproval: true,
		Description: "Play the connected device's microphone through this computer's speakers. Starts the bundled Wendy CLI in the background and returns a job_id promptly. Chat remains usable; this does not feed audio to the model. Uses the current direct or cloud connection target and selects a usable microphone when device_id is omitted. Check background_process_list for startup errors. Repeated identical requests reuse a running listener. Stops on background_process_stop or chat exit.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device_id":{"type":"integer","minimum":0,"maximum":4294967295,"description":"Remote audio capture device ID; 0 or omitted auto-selects a usable microphone"},"sample_rate":{"type":"integer","minimum":8000,"maximum":192000,"description":"Sample rate in Hz; default 16000"},"channels":{"type":"integer","minimum":1,"maximum":2,"description":"Default 1"},"buffer_ms":{"type":"integer","minimum":20,"maximum":2000,"description":"Playback jitter buffer in milliseconds; default 150"}},"additionalProperties":false}`),
	},
	{
		Name:        "background_process_list",
		Description: "List this chat's background camera viewers and audio listeners, including job_id, process state, device and exit status. Provide job_id for recent bounded output and failure diagnostics. Running means the child process exists, not that playback has started. Completed history is bounded to 16 jobs.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string","minLength":1,"maxLength":64}},"additionalProperties":false}`),
	},
	{
		Name:        "background_process_stop",
		Description: "Stop a background camera viewer or audio listener started by this chat, including its player subprocesses. Use a job_id from camera_view, audio_listen or background_process_list. Cannot stop arbitrary system processes. Already exited jobs are returned unchanged.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string","minLength":1,"maxLength":64}},"required":["job_id"],"additionalProperties":false}`),
	},
}

// This is a replayable target supplied by the bundled MCP server, never argv
// or executable text supplied by the model or a device application.
type backgroundTarget struct {
	Device    string `json:"device"`
	Transport string `json:"transport"`
	CloudGRPC string `json:"cloud_grpc,omitempty"`
	BrokerURL string `json:"broker_url,omitempty"`
}

func (t *Tools) backgroundTarget(ctx context.Context) (backgroundTarget, error) {
	if t.mcp == nil {
		return backgroundTarget{}, errors.New("connect to a device before starting local playback")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request := mcpgo.CallToolRequest{}
	request.Params.Name, request.Params.Arguments = "wendy_status", map[string]any{}
	result, err := t.mcp.CallTool(ctx, request)
	if err != nil {
		return backgroundTarget{}, fmt.Errorf("reading current device connection: %w", err)
	}
	if result == nil || result.IsError {
		return backgroundTarget{}, errors.New("could not read the current device connection")
	}
	var data []byte
	if result.StructuredContent != nil {
		data, err = json.Marshal(result.StructuredContent)
	} else {
		for _, item := range result.Content {
			switch value := item.(type) {
			case mcpgo.TextContent:
				data = []byte(value.Text)
			case *mcpgo.TextContent:
				if value != nil {
					data = []byte(value.Text)
				}
			}
			if data != nil {
				break
			}
		}
	}
	var connection struct {
		Connected bool              `json:"connected"`
		Target    *backgroundTarget `json:"command_target"`
	}
	if err != nil || json.Unmarshal(data, &connection) != nil || !connection.Connected || connection.Target == nil {
		return backgroundTarget{}, errors.New("no replayable device connection; connect with device_connect or cloud_connect before opening playback")
	}
	target := *connection.Target
	if err := target.validate(); err != nil {
		return backgroundTarget{}, err
	}
	return target, nil
}

func (target backgroundTarget) validate() error {
	for _, value := range []string{target.Device, target.CloudGRPC, target.BrokerURL} {
		if len(value) > 512 || strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.TrimSpace(value) != value {
			return errors.New("the current device has an invalid local playback target")
		}
	}
	if target.Device == "" || strings.HasPrefix(target.Device, "-") {
		return errors.New("the current device has no usable local playback target")
	}
	if target.Transport != "direct" && target.Transport != "cloud" {
		return fmt.Errorf("local playback cannot replay transport %q", target.Transport)
	}
	if target.Transport == "direct" && (target.CloudGRPC != "" || target.BrokerURL != "") {
		return errors.New("direct playback target contains cloud options")
	}
	if target.Transport == "cloud" && target.CloudGRPC == "" {
		return errors.New("cloud playback requires the connected cloud endpoint")
	}
	return nil
}

func backgroundArgs(kind string, target backgroundTarget, raw json.RawMessage) ([]string, error) {
	if err := target.validate(); err != nil {
		return nil, err
	}
	args := []string{"--device=" + target.Device}
	if target.Transport == "cloud" {
		args = append(args, "cloud")
	}
	args = append(args, "device")
	if target.Transport == "cloud" {
		args = append(args, "--cloud-grpc="+target.CloudGRPC)
		// An explicit empty value preserves MCP's endpoint-derived default
		// instead of inheriting a different WENDY_BROKER_URL in the child CLI.
		args = append(args, "--broker-url="+target.BrokerURL)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	var fields []struct{ key, flag string }
	switch kind {
	case "camera_view":
		args = append(args, "camera", "view", "--non-interactive")
		if _, hasID := values["camera_id"]; hasID && values["stable_id"] != nil {
			return nil, errors.New("provide camera_id or stable_id, not both")
		}
		if value, ok := values["stable_id"]; ok {
			var id string
			if json.Unmarshal(value, &id) != nil || id == "" || strings.IndexFunc(id, unicode.IsControl) >= 0 {
				return nil, errors.New("stable_id must be a nonempty camera identifier without control characters")
			}
			args = append(args, "--stable-id="+id)
		}
		fields = []struct{ key, flag string }{{"camera_id", "id"}, {"width", "width"}, {"height", "height"}, {"fps", "fps"}}
	case "audio_listen":
		args = append(args, "audio", "listen", "--non-interactive")
		fields = []struct{ key, flag string }{{"device_id", "id"}, {"sample_rate", "sample-rate"}, {"channels", "channels"}, {"buffer_ms", "buffer-ms"}}
	default:
		return nil, fmt.Errorf("unsupported background command %q", kind)
	}
	for _, field := range fields {
		if value, ok := values[field.key]; ok {
			var n json.Number
			if json.Unmarshal(value, &n) != nil {
				return nil, fmt.Errorf("%s must be an integer", field.key)
			}
			f, err := strconv.ParseFloat(string(n), 64)
			if err != nil || f < 0 || f > 4294967295 || f != float64(uint32(f)) {
				return nil, fmt.Errorf("%s must be an unsigned integer", field.key)
			}
			args = append(args, "--"+field.flag+"="+strconv.FormatUint(uint64(f), 10))
		}
	}
	return args, nil
}

func (t *Tools) executeBackground(ctx context.Context, call ToolCall) (string, error) {
	if t.background == nil {
		return "", errors.New("background playback is unavailable before the chat tool session starts")
	}
	var value any
	var err error
	switch call.Name {
	case "camera_view", "audio_listen":
		target, targetErr := t.backgroundTarget(ctx)
		if targetErr != nil {
			return "", targetErr
		}
		args, argsErr := backgroundArgs(call.Name, target, call.Arguments)
		if argsErr != nil {
			return "", argsErr
		}
		value, err = t.background.start(ctx, call.Name, target, args)
	case "background_process_list", "background_process_stop":
		var args struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return "", err
		}
		if call.Name == "background_process_stop" {
			value, err = t.background.stop(args.JobID)
		} else {
			var jobs []backgroundJobInfo
			jobs, err = t.background.list(args.JobID)
			value = map[string]any{"jobs": jobs}
		}
	}
	encoded, marshalErr := json.Marshal(value)
	return string(encoded), errors.Join(err, marshalErr)
}
