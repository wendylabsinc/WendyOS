package commands

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// No environment proxy, saved API key, public HTTP listener or hosted fallback.
// Model requests use the same authenticated container stream as desktop chat.
type chatModelTransport struct {
	conn *grpcclient.AgentConnection
	port uint32
}

func (t chatModelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.conn == nil || (!t.conn.IsMTLS && !t.conn.IsSessionProxy && t.conn.RegistryDialer == nil) {
		return nil, fmt.Errorf("local inference requires an authenticated Wendy device connection")
	}
	return DesktopModelRoundTrip(req, t.conn, t.port, 2*time.Minute)
}

var chatModelConnect = DesktopConnect

func connectChatModel(ctx context.Context, device string, port uint32, cfg chat.Config) (chat.Config, func(chat.Config) (chat.Provider, error), func(), error) {
	fail := func(err error) (chat.Config, func(chat.Config) (chat.Provider, error), func(), error) {
		return chat.Config{}, nil, nil, err
	}
	if port == 0 || port > 65535 {
		return fail(fmt.Errorf("--model-port must be from 1 to 65535"))
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return fail(fmt.Errorf("--model-device requires --model with an installed tool-capable model name"))
	}
	if cfg.MaxTokens < 0 {
		return fail(fmt.Errorf("--max-tokens must be positive"))
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := chatModelConnect(probe, device)
	if err != nil {
		return fail(err)
	}
	transport := chatModelTransport{conn: conn, port: port}
	models, err := chat.DeviceModels(probe, transport)
	if err != nil {
		_ = conn.Close()
		return fail(fmt.Errorf("local model discovery: %w", err))
	}
	found := false
	for _, available := range models {
		if available.ID == model {
			found = true
			break
		}
	}
	if !found {
		_ = conn.Close()
		return fail(fmt.Errorf("model %q is not installed on the selected local runtime", model))
	}
	local := chat.Config{Provider: "local", Model: model, BaseURL: "http://wendy-device/v1", MaxTokens: cfg.MaxTokens}
	factory := func(c chat.Config) (chat.Provider, error) {
		if c.Model != model || c.Provider != "local" || c.BaseURL != local.BaseURL || c.APIKey != "" {
			return nil, fmt.Errorf("this session is pinned to its selected local model")
		}
		return chat.DeviceProviderWithLimit(model, transport, cfg.MaxTokens)
	}
	return local, factory, func() { _ = conn.Close() }, nil
}
