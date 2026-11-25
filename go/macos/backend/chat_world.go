package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

const showWorldWorkspaceTool = "show_world_workspace"

func worldWorkspaceTool() chat.Tool {
	return chat.Tool{
		Name:        showWorldWorkspaceTool,
		Description: "Display a native card that lets the user open World operations, missions, annotations, areas, or sessions in this app. This only presents a workspace shortcut. The user selects their imported World profile and reviews any changes in the workspace. A simulation profile is separate from the physical device selected for Chat. No mission is started, no observation is fetched, and no authority is granted by this tool. Do not pass credentials, profile paths, endpoints, or execution arguments.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"section":{"type":"string","enum":["operations","missions","annotations","areas","sessions"]}},"required":["section"],"additionalProperties":false}`),
	}
}

// World cards use Episode as a section identifier across the existing native
// bridge. It is not an episode, profile, endpoint, credential, or command.
func (t *deviceCardTools) showWorldWorkspace(ctx context.Context, call chat.ToolCall) (string, error) {
	var args struct {
		Section string `json:"section"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("expected one World workspace request")
	}
	var title string
	switch args.Section {
	case "operations":
		title = "World operations"
	case "missions":
		title = "World missions"
	case "annotations":
		title = "World annotations"
	case "areas":
		title = "World areas"
	case "sessions":
		title = "World sessions"
	default:
		return "", errors.New("choose World operations, missions, annotations, areas, or sessions")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.turn == nil {
		return "", errors.New("the chat response is unavailable")
	}
	t.turn.mu.Lock()
	defer t.turn.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	shown := false
	for _, card := range t.turn.Cards {
		if card.Kind == "world" && card.Episode == args.Section {
			shown = true
			break
		}
	}
	if !shown {
		if len(t.turn.Cards) >= 6 {
			return "", errors.New("this response already has six cards")
		}
		t.turn.Cards = append(t.turn.Cards, ChatCard{Kind: "world", Title: title, Episode: args.Section})
	}
	return fmt.Sprintf("Displayed a shortcut to World %s. The user can open it, select their imported World profile, and review changes in the workspace. No workspace data was read and no mission was executed or authority granted. A simulation profile is separate from the physical device selected for Chat.", args.Section), nil
}
