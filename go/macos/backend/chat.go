package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
)

type ChatTurn struct {
	Cards                       []ChatCard
	mu                          sync.Mutex
	cancel                      context.CancelFunc
	ID                          uint64
	Text, Error                 string
	Done                        bool
	Activity                    string
	Trace                       string
	ApprovalID                  uint64
	ApprovalAction, ApprovalApp string
	approvalReply               chan bool
	nextApproval                uint64
}
type ChatSnapshot struct {
	Cards                       []ChatCard
	Text, Error                 string
	Done                        bool
	Activity                    string
	Trace                       string
	ApprovalID                  uint64
	ApprovalAction, ApprovalApp string
}

// All HTTP requests are fixed to one selected device and port. A proxy from the
// Mac's environment cannot intercept these requests; redirects are refused.
func deviceChatTransport(conn *grpcclient.AgentConnection, port uint32) (*chatTransport, error) {
	if port == 0 || port > 65535 {
		return nil, errors.New("enter a port from 1 to 65535")
	}
	return &chatTransport{conn: conn, port: port, Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Minute, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "wendy-device:80" {
			return nil, errors.New("chat requests must stay on the selected device")
		}
		if conn.RegistryDialer != nil {
			return conn.RegistryDialer(ctx, int(port))
		}
		host := strings.Trim(conn.Host, "[]")
		if host == "" || strings.HasPrefix(host, "unix:") {
			return nil, errors.New("device has no model server address")
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	}}}, nil
}

type chatTransport struct {
	*http.Transport
	conn *grpcclient.AgentConnection
	port uint32
}

func (t *chatTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(t.port)))
	if t.conn.RegistryDialer == nil {
		if !t.conn.IsMTLS && !t.conn.IsSessionProxy {
			return nil, errors.New("model access requires an authenticated device connection")
		}
		// Reuse the model HTTP tunnel over the authenticated Agent exec stream.
		// LAN connections must not send prompts to an unauthenticated HTTP port.
		return commands.DesktopModelRoundTrip(req, t.conn, t.port, t.ResponseHeaderTimeout)
	}
	response, err := t.Transport.RoundTrip(req)
	var unsupported *cloudrelay.UnsupportedServiceError
	if !errors.As(err, &unsupported) || req.Context().Err() != nil {
		return response, err
	}
	// Only an unsupported service can take this path, before any HTTP bytes
	// reach the device. Never retry an authorization failure or a sent request.
	retry := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, err
		}
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
		defer retry.Body.Close()
	}
	response, fallbackErr := commands.DesktopModelRoundTrip(retry, t.conn, t.port, t.ResponseHeaderTimeout)
	if fallbackErr != nil {
		return nil, fmt.Errorf("Cloud model connection failed: %w", errors.Join(unsupported, fallbackErr))
	}
	return response, nil
}

type DeviceChatModel struct {
	chat.ModelOption
	Port              uint32
	UnavailableReason string
}

func (b *Backend) ChatModels(session uint64, port uint32) ([]DeviceChatModel, error) {
	b.mu.Lock()
	if session == 0 {
		b.mu.Unlock()
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(b.life, 15*time.Second)
	b.mu.Unlock()
	defer cancel()
	if port == 0 || port > 65535 {
		return nil, errors.New("enter a port from 1 to 65535")
	}
	inventoryCtx, inventoryCancel := context.WithTimeout(ctx, 3*time.Second)
	ports, _ := commands.DesktopModelPorts(inventoryCtx, conn)
	inventoryCancel()
	known := map[uint32]bool{}
	for _, p := range ports {
		known[p] = true
	}
	return discoverDeviceChatModels(ctx, port, ports, func(ctx context.Context, port uint32) ([]DeviceChatModel, error) {
		transport, err := deviceChatTransport(conn, port)
		if err != nil {
			return nil, err
		}
		defer transport.CloseIdleConnections()
		httpCtx, httpCancel := context.WithTimeout(ctx, 4*time.Second)
		models, queryErr := chat.DeviceModels(httpCtx, transport)
		httpCancel()
		var result []DeviceChatModel
		for _, model := range models {
			result = append(result, DeviceChatModel{ModelOption: model})
		}
		if queryErr == nil || !known[port] {
			return result, queryErr
		}
		names, inventoryErr := commands.DesktopDownloadedModelNames(ctx, conn, port)
		if inventoryErr != nil {
			return nil, queryErr
		}
		for _, name := range names {
			result = append(result, DeviceChatModel{ModelOption: chat.ModelOption{ID: name, Name: name}, UnavailableReason: fmt.Sprintf("The model is downloaded, but its server on port %d could not be reached. Check that the model app is running, then retry.\n\n%v", port, queryErr)})
		}
		return result, nil
	})
}

// Query known runtimes concurrently so an unavailable default server does not
// hide downloaded models on another port. Prefer the requested server on duplicates.
func discoverDeviceChatModels(ctx context.Context, preferred uint32, installed []uint32, query func(context.Context, uint32) ([]DeviceChatModel, error)) ([]DeviceChatModel, error) {
	ports := []uint32{preferred}
	seenPorts := map[uint32]bool{preferred: true}
	for _, port := range installed {
		if port > 0 && port <= 65535 && !seenPorts[port] {
			ports = append(ports, port)
			seenPorts[port] = true
		}
	}
	results := make([][]DeviceChatModel, len(ports))
	errs := make([]error, len(ports))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, port := range ports {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			results[i], errs[i] = query(probeCtx, port)
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var models []DeviceChatModel
	seen := map[string]int{}
	for i, list := range results {
		for _, model := range list {
			model.Port = ports[i]
			if index, exists := seen[model.ID]; !exists {
				seen[model.ID] = len(models)
				models = append(models, model)
			} else if models[index].UnavailableReason != "" && model.UnavailableReason == "" {
				models[index] = model
			}
		}
	}
	if len(models) > 0 {
		return models, nil
	}
	if errs[0] != nil {
		return nil, errs[0]
	}
	return nil, chat.ErrNoModels
}

func (b *Backend) StartChat(session, turn uint64, port uint32, model string, messages []chat.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 || turn == 0 {
		return errors.New("a device session and conversation turn are required")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if len(messages) == 0 || len(messages) > 200 {
		return errors.New("start a new conversation after 200 messages")
	}
	if messages[len(messages)-1].Role != "user" {
		return errors.New("the last message must be from the user")
	}
	size := 0
	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			return errors.New("invalid chat message role")
		}
		size += len(msg.Content)
	}
	if size > 1<<20 {
		return errors.New("this conversation is too large; start a new chat")
	}
	transport, err := deviceChatTransport(conn, port)
	if err != nil {
		return err
	}
	provider, err := chat.DeviceProvider(model, transport)
	if err != nil {
		return err
	}
	if b.chatTurn != nil {
		b.chatTurn.cancel()
	}
	ctx, cancel := context.WithTimeout(b.life, 15*time.Minute)
	state := &ChatTurn{ID: turn, cancel: cancel, Activity: "Connecting Wendy tools"}
	if b.chatEnvironment == nil {
		factory := b.chatToolsFactory
		if factory == nil {
			factory = bundledChatTools
		}
		selector := b.deviceSelector
		if selector == "" {
			selector = conn.Addr
		}
		if selector == "" && b.chatToolsFactory == nil {
			cancel()
			return errors.New("reconnect the device before starting chat")
		}
		b.chatEnvironment = newDesktopChatEnvironment(b.life, selector, factory)
	}
	environment := b.chatEnvironment
	b.chatTurn = state
	go func() {
		defer cancel()
		defer transport.CloseIdleConnections()
		executor, err := environment.get(ctx)
		if err != nil {
			state.mu.Lock()
			state.Done = true
			state.Error = err.Error()
			state.Activity = ""
			state.mu.Unlock()
			return
		}
		engine := chat.NewEngine(deviceHistoryProvider{Provider: provider, history: messages[:len(messages)-1]}, &deviceCardTools{Executor: &deviceCampaignTools{Executor: executor, conn: conn, approveNotifications: state.approve}, conn: conn, turn: state}, deviceChatPrompt())
		err = engine.Turn(ctx, messages[len(messages)-1].Content, func(event chat.Event) {
			state.mu.Lock()
			defer state.mu.Unlock()
			switch event.Type {
			case "text":
				if len(state.Text)+len(event.Text) > 1<<20 {
					state.Error = "The response exceeded the display limit."
					cancel()
					return
				}
				state.Text += event.Text
			case "status":
				state.Activity = event.Text
			case "tool_result":
				if event.Call != nil && len(state.Trace) < 64*1024 {
					state.Trace += event.Call.Name + "\n" + event.Text + "\n\n"
				}
			case "tool_start":
				if event.Call != nil {
					state.Activity = "Using " + event.Call.Name
				}
			}
		}, state.approve)

		state.mu.Lock()
		defer state.mu.Unlock()
		state.Done = true
		if err != nil && !errors.Is(err, context.Canceled) && state.Error == "" {
			state.Error = err.Error()
		}
		state.Activity = ""

	}()
	return nil
}
func (b *Backend) PollChat(session, turn uint64) (ChatSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 {
		return ChatSnapshot{}, errors.New("select a device first")
	}
	if _, err := b.connection(session); err != nil {
		return ChatSnapshot{}, err
	}
	if b.chatTurn == nil || b.chatTurn.ID != turn {
		return ChatSnapshot{}, fmt.Errorf("conversation turn changed")
	}
	b.chatTurn.mu.Lock()
	defer b.chatTurn.mu.Unlock()
	return ChatSnapshot{Cards: append([]ChatCard(nil), b.chatTurn.Cards...), Text: b.chatTurn.Text, Error: b.chatTurn.Error, Done: b.chatTurn.Done, Activity: b.chatTurn.Activity, Trace: b.chatTurn.Trace, ApprovalID: b.chatTurn.ApprovalID, ApprovalAction: b.chatTurn.ApprovalAction, ApprovalApp: b.chatTurn.ApprovalApp}, nil
}
func (b *Backend) StopChat(session, turn uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 {
		return errors.New("select a device first")
	}
	if _, err := b.connection(session); err != nil {
		return err
	}
	if b.chatTurn != nil && b.chatTurn.ID == turn {
		b.chatTurn.cancel()
	}
	return nil
}

// An approval is scoped to a single immutable call, turn and device session.
func (s *ChatTurn) approve(ctx context.Context, call chat.ToolCall) (bool, error) {
	if len(call.Arguments) > 2<<20 {
		return false, errors.New("tool arguments exceed display limit")
	}
	var pretty json.RawMessage = call.Arguments
	var args any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return false, err
	}
	pretty, _ = json.MarshalIndent(args, "", "  ")
	reply := make(chan bool, 1)
	s.mu.Lock()
	s.nextApproval++
	s.ApprovalID = s.nextApproval
	s.ApprovalAction = call.Name
	s.ApprovalApp = string(pretty)
	s.approvalReply = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.ApprovalID = 0
		s.ApprovalAction = ""
		s.ApprovalApp = ""
		s.approvalReply = nil
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case allowed := <-reply:
		return allowed, ctx.Err()
	}
}
func (b *Backend) ResolveChatApproval(session, turn, approval uint64, allow bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 {
		return errors.New("select a device first")
	}
	if _, err := b.connection(session); err != nil {
		return err
	}
	s := b.chatTurn
	if s == nil || s.ID != turn {
		return errors.New("conversation turn changed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if approval == 0 || s.ApprovalID != approval || s.approvalReply == nil || s.Done {
		return errors.New("this action is no longer pending")
	}
	s.approvalReply <- allow
	s.approvalReply = nil
	return nil
}
