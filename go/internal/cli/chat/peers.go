package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/a2a"
)

type PeerSpec struct {
	URL         string `json:"url"`
	TokenEnv    string `json:"token_env,omitempty"`
	Description string `json:"description,omitempty"`
}

func ValidatePeers(peers map[string]PeerSpec) error {
	if len(peers) > 100 {
		return errors.New("at most 100 agent peers are supported")
	}
	for name, peer := range peers {
		if strings.TrimSpace(name) == "" || len(name) > 100 {
			return errors.New("peer names must be 1-100 characters")
		}
		if err := a2a.ValidateURL(peer.URL); err != nil {
			return fmt.Errorf("peer %s: %w", name, err)
		}
		if err := a2a.ValidateToken(os.Getenv(peer.TokenEnv)); err != nil {
			return fmt.Errorf("peer %s token environment variable: %w", name, err)
		}
	}
	return nil
}
func LoadPeers(file string) (map[string]PeerSpec, error) {
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var peers map[string]PeerSpec
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&peers); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("agent peers must contain one JSON object")
	}
	return peers, ValidatePeers(peers)
}

var remoteTool = Tool{Name: "agent_remote", RequiresApproval: true, Description: "Submit, inspect, or cancel a task on an explicitly configured A2A peer. Send requires a stable request_id, reused only when retrying that same task, and returns a durable task ID immediately; poll get for results. Remote work survives this chat and uses the remote service's own tool policy. Cancellation is explicit and does not undo completed effects. Never send credentials or unrelated private context.", Parameters: json.RawMessage(`{"type":"object","properties":{"peer":{"type":"string","minLength":1},"action":{"type":"string","enum":["send","get","cancel"]},"prompt":{"type":"string","maxLength":16000},"task_id":{"type":"string","maxLength":256},"request_id":{"type":"string","minLength":1,"maxLength":256}},"required":["peer","action"],"additionalProperties":false}`)}

func (s *agentSupervisor) remote(ctx context.Context, call ToolCall) (string, error) {
	if err := validateArguments(remoteTool, call.Arguments); err != nil {
		return "", err
	}
	var args struct {
		Peer      string `json:"peer"`
		Action    string `json:"action"`
		Prompt    string `json:"prompt"`
		TaskID    string `json:"task_id"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	peer, ok := s.options.Peers[args.Peer]
	if !ok {
		return "", fmt.Errorf("unknown configured peer %q", args.Peer)
	}
	client, err := a2a.NewClient(peer.URL, os.Getenv(peer.TokenEnv))
	if err != nil {
		return "", err
	}
	var result any
	switch args.Action {
	case "send":
		if s.options.DelegationDepth >= a2a.MaxDelegationDepth {
			return "", errors.New("remote delegation depth reached four hops")
		}
		if strings.TrimSpace(args.Prompt) == "" || strings.TrimSpace(args.RequestID) == "" || args.TaskID != "" {
			return "", errors.New("send requires prompt and request_id, and no task_id")
		}
		req := a2a.SendRequest{Message: a2a.Message{MessageID: args.RequestID, Role: "ROLE_USER", Parts: []a2a.Part{{Text: args.Prompt}}}}
		req.Metadata = map[string]any{a2a.DelegationDepthKey: s.options.DelegationDepth + 1}
		req.Configuration.ReturnImmediately = true
		result, err = client.Send(ctx, req)
	case "get", "cancel":
		if args.TaskID == "" || args.Prompt != "" {
			return "", errors.New("get/cancel requires task_id and no prompt")
		}
		if args.Action == "get" {
			result, err = client.Get(ctx, args.TaskID)
		} else {
			result, err = client.Cancel(ctx, args.TaskID)
		}
	}
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	return string(data), err
}
func (s *agentSupervisor) peerTool() Tool {
	tool := remoteTool
	names := make([]string, 0, len(s.options.Peers))
	for name := range s.options.Peers {
		names = append(names, name)
	}
	sort.Strings(names)
	tool.Description += " Configured peers:"
	for _, name := range names {
		tool.Description += "\n" + name + ": " + boundedText(s.options.Peers[name].Description, 500)
	}
	return tool
}
