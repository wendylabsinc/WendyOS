package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"io"
	"strings"
	"unicode"
)

// A robot_find card carries the object in Title and "bark" as Camera 1.
// A world card carries its workspace section in Episode.
type ChatCard struct {
	Kind, Title string
	Episode     string
	Camera      uint32
}

const robotFindTool = "find_with_robot"

type deviceCardTools struct {
	chat.Executor
	conn *grpcclient.AgentConnection
	turn *ChatTurn
}

func (t *deviceCardTools) ListTools(ctx context.Context) ([]chat.Tool, error) {
	tools, err := t.Executor.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	return append(tools, worldWorkspaceTool(), chat.Tool{Name: "show_device_card", Description: "Display a native interactive card in this conversation for the originally selected device. Use camera for a live camera player. For a named camera pass camera_name (for example ESP), or a known camera_id. Omitting a selector never shows multiple cameras; set all_cameras true only when the user explicitly asks for all cameras, resources for live CPU/memory/GPU, or device_info for hardware and software versions. The card reads current device data, not model-generated values. Use notifications to show the native permission and Cloud push registration prompt on this Mac or iPhone. Displaying it does not grant permission or verify registration. Camera playback does not give the model visual perception.", Parameters: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["camera","resources","device_info","notifications"]},"camera_id":{"type":"integer","minimum":0},"camera_name":{"type":"string","minLength":1},"all_cameras":{"type":"boolean"}},"required":["kind"],"additionalProperties":false}`)},
		chat.Tool{Name: robotFindTool, RequiresApproval: true, Description: "Send the Unitree Go2 robot dog paired with the originally selected device to find a physical object. Use it when the user asks the robot or dog to find, look for, locate or go to something, including when they do not know which room it is in. Go2 first returns to where it last saw the object, then looks around with its camera and LiDAR and walks to other places until it sees it, then walks up to it. This is physical motion and needs the user's approval. It displays a live third-person view of Go2 in this conversation; the search runs there, stops if the user closes that view, and reports its own result. The tool returns once the view is shown, not when the object is found, so never claim the object was found. Set bark true only when the user asks for a bark or sound on finding it.", Parameters: json.RawMessage(`{"type":"object","properties":{"object":{"type":"string","minLength":1,"maxLength":80,"description":"The thing to find, in a few words, such as red toolbox"},"bark":{"type":"boolean"}},"required":["object"],"additionalProperties":false}`)}), nil
}

func (t *deviceCardTools) findWithRobot(ctx context.Context, call chat.ToolCall) (string, error) {
	var args struct {
		Object string `json:"object"`
		Bark   bool   `json:"bark"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", err
	}
	object := strings.Join(strings.Fields(args.Object), " ")
	if object == "" || len(object) > 80 {
		return "", errors.New("name the object to find in a few words")
	}
	if t.conn == nil || t.conn.UnitreeService == nil {
		return "", errors.New("reconnect the original chat device")
	}
	listed, err := t.conn.UnitreeService.ListUnitreePairings(ctx, &pbv2.ListUnitreePairingsRequest{})
	if err != nil {
		return "", err
	}
	connected := false
	for _, robot := range listed.GetRobots() {
		connected = connected || robot.GetPaired() && robot.GetConnected()
	}
	if !connected {
		return "", errors.New("no Go2 is paired and connected on this device; pair one under Pairing > Unitree")
	}
	card := ChatCard{Kind: "robot_find", Title: object}
	if args.Bark {
		card.Camera = 1
	}
	t.turn.mu.Lock()
	defer t.turn.mu.Unlock()
	for _, shown := range t.turn.Cards {
		if shown.Kind == card.Kind {
			return "", errors.New("this response already started a robot search")
		}
	}
	if len(t.turn.Cards) >= 6 {
		return "", errors.New("this response already has six cards")
	}
	t.turn.Cards = append(t.turn.Cards, card)
	return fmt.Sprintf("Displayed the live Go2 view. It is now connecting to Go2 and will search for %q there; the view shows progress and the outcome. The object has not been found yet. Tell the user to keep the view open, and that Stop is on the view.", object), nil
}
func (t *deviceCardTools) Execute(ctx context.Context, call chat.ToolCall) (string, error) {
	if call.Name == showWorldWorkspaceTool {
		return t.showWorldWorkspace(ctx, call)
	}
	if call.Name == robotFindTool {
		return t.findWithRobot(ctx, call)
	}
	if call.Name != "show_device_card" {
		return t.Executor.Execute(ctx, call)
	}
	var args struct {
		Kind       string  `json:"kind"`
		Camera     *uint32 `json:"camera_id"`
		CameraName string  `json:"camera_name"`
		AllCameras bool    `json:"all_cameras"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("expected one card request")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if args.AllCameras && (args.Camera != nil || args.CameraName != "") {
		return "", errors.New("choose one camera or all cameras, not both")
	}
	var cards []ChatCard
	switch args.Kind {
	case "notifications":
		cards = []ChatCard{{Kind: "notifications", Title: "Receive Cloud notifications"}}
	case "resources":
		cards = []ChatCard{{Kind: "resources", Title: "Device activity"}}
	case "device_info":
		cards = []ChatCard{{Kind: "device_info", Title: "Device info"}}
	case "camera":
		if t.conn == nil || t.conn.VideoService == nil {
			return "", errors.New("reconnect the original chat device")
		}
		result, err := t.conn.VideoService.ListVideoDevices(ctx, &pb.ListVideoDevicesRequest{})
		if err != nil {
			return "", err
		}
		for _, camera := range result.GetDevices() {
			if args.Camera != nil && *args.Camera != camera.GetId() {
				continue
			}
			if args.CameraName != "" && !cameraNameMatches(camera.GetName(), args.CameraName) {
				continue
			}
			cards = append(cards, ChatCard{Kind: "camera", Title: camera.GetName(), Camera: camera.GetId()})
		}
		if len(cards) > 1 && !args.AllCameras {
			choices, _ := json.Marshal(cards)
			return "", fmt.Errorf("choose one camera using camera_id or a more specific camera_name; available matches: %s", choices)
		}
		if len(cards) > 4 {
			cards = cards[:4]
		}

		if len(cards) == 0 {
			return "", errors.New("no matching camera is available on the selected device")
		}
	default:
		return "", errors.New("unsupported card type")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	t.turn.mu.Lock()
	defer t.turn.mu.Unlock()
	for _, card := range cards {
		duplicate := false
		for _, shown := range t.turn.Cards {
			if shown.Kind == card.Kind && shown.Camera == card.Camera {
				duplicate = true
			}
		}
		if !duplicate {
			if len(t.turn.Cards) >= 6 {
				return "", errors.New("this response already has six cards")
			}
			t.turn.Cards = append(t.turn.Cards, card)
		}
	}
	shown, _ := json.Marshal(cards)
	return fmt.Sprintf("Displayed native cards: %s. Camera playback and notification registration are controlled by the user. Displaying a card does not enable notifications.", shown), nil
}

// Match user-facing camera names without treating punctuation as word boundaries
// supplied by the model ("ESP camera" matches "esp-camera-02:camera").
func cameraNameMatches(name, query string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(tokens) == 0 {
		return false
	}
	name = strings.ToLower(name)
	for _, token := range tokens {
		if !strings.Contains(name, token) {
			return false
		}
	}
	return true
}

func (t *deviceCardTools) ExecuteResult(ctx context.Context, call chat.ToolCall) (chat.ToolResult, error) {
	if call.Name != "show_device_card" && call.Name != robotFindTool && call.Name != showWorldWorkspaceTool {
		if media, ok := t.Executor.(chat.MediaExecutor); ok {
			return media.ExecuteResult(ctx, call)
		}
	}
	text, err := t.Execute(ctx, call)
	return chat.ToolResult{Text: text}, err
}
