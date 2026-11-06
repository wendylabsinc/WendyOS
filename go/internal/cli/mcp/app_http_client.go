package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// appHTTPClient adapts only app-facing HTTP. The server, stdio and embedded
// clients retain mcp-go's types and transports.
type appHTTPClient struct {
	ctx        context.Context
	cancel     context.CancelFunc
	session    *sdk.ClientSession
	transport  *http.Transport
	closeProxy func()
	once       sync.Once
	closeErr   error
}

func newAppHTTPClient(lifetimeCtx, initCtx context.Context, endpoint string, closeProxy func()) (*appHTTPClient, error) {
	ctx, cancel := context.WithCancel(lifetimeCtx)
	initCtx, cancelInit := context.WithCancel(initCtx)
	stop := context.AfterFunc(ctx, cancelInit)
	defer stop()
	defer cancelInit()
	if ctx.Err() != nil {
		cancelInit()
	}
	// App responses must never redirect requests outside the local TCP proxy.
	transport := &http.Transport{}
	client := sdk.NewClient(&sdk.Implementation{Name: "wendy-app-proxy", Version: "1"},
		&sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}})
	session, err := client.Connect(initCtx, &sdk.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, &sdk.ClientSessionOptions{ProtocolVersion: mcpgo.LATEST_PROTOCOL_VERSION})
	if err != nil {
		cancel()
		closeProxy()
		transport.CloseIdleConnections()
		return nil, err
	}
	return &appHTTPClient{ctx: ctx, cancel: cancel, session: session, transport: transport, closeProxy: closeProxy}, nil
}

func (c *appHTTPClient) ListTools(ctx context.Context, req mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	ctx, done := c.callContext(ctx)
	defer done()
	params := &sdk.ListToolsParams{Cursor: string(req.Params.Cursor)}
	var result *mcpgo.ListToolsResult
	for {
		page, err := c.session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		var converted mcpgo.ListToolsResult
		if err := appMCPJSON(page, &converted); err != nil {
			return nil, err
		}
		if result == nil {
			result = &converted
		} else {
			result.Tools = append(result.Tools, converted.Tools...)
			result.NextCursor = converted.NextCursor
		}
		if page.NextCursor == "" {
			return result, nil
		}
		params.Cursor = page.NextCursor
	}
}

func (c *appHTTPClient) CallTool(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	ctx, done := c.callContext(ctx)
	defer done()
	result, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("adapting app MCP response: %w", err)
	}
	raw := json.RawMessage(data)
	return mcpgo.ParseCallToolResult(&raw)
}

// Convert through the MCP wire representation, including schemas, annotations,
// structured results and all supported content variants.
func appMCPJSON(from, to any) error {
	data, err := json.Marshal(from)
	if err == nil {
		err = json.Unmarshal(data, to)
	}
	if err != nil {
		return fmt.Errorf("adapting app MCP response: %w", err)
	}
	return nil
}

// Calls belong to both the caller and the app generation. Cancel them before
// the SDK's graceful close, which waits for outstanding RPCs.
func (c *appHTTPClient) callContext(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	if c.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func (c *appHTTPClient) Close() error {
	c.once.Do(func() {
		c.cancel()
		// The SDK bounds its session termination request. Keep the proxy alive
		// until it completes, then join all proxy streams.
		c.closeErr = c.session.Close()
		c.closeProxy()
		c.transport.CloseIdleConnections()
	})
	return c.closeErr
}
