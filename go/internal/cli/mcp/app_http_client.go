package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A var so tests need not wait out the production timeout.
var appHTTPIdleConnTimeout = 90 * time.Second

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
	client := sdk.NewClient(&sdk.Implementation{Name: "wendy-app-proxy", Version: "1"},
		&sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}})
	return newAppHTTPClientWithSDK(lifetimeCtx, initCtx, endpoint, closeProxy, client)
}

func newAppHTTPClientWithSDK(lifetimeCtx, initCtx context.Context, endpoint string, closeProxy func(), client *sdk.Client) (*appHTTPClient, error) {
	ctx, cancel := context.WithCancel(lifetimeCtx)
	initCtx, cancelInit := context.WithCancel(initCtx)
	stop := context.AfterFunc(ctx, cancelInit)
	defer stop()
	defer cancelInit()
	if ctx.Err() != nil {
		cancelInit()
	}
	// App responses must never redirect requests outside the local TCP proxy.
	transport := &http.Transport{IdleConnTimeout: appHTTPIdleConnTimeout}
	closeProxy = sync.OnceFunc(closeProxy)
	// A canceled initialize can leave the SDK waiting for its best-effort
	// cancellation notification during graceful session cleanup. Close the
	// proxy on cancellation, before Connect must return, so that notification
	// cannot hold cleanup open. Stop this before our deferred cancelInit on a
	// successful handshake; initialization no longer owns the live session.
	stopClosingProxy := context.AfterFunc(initCtx, closeProxy)
	defer stopClosingProxy()
	// Keep the app proxy on the session-based protocol: cancellation and
	// shutdown depend on initialize and session termination.
	session, err := client.Connect(initCtx, &sdk.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, &sdk.ClientSessionOptions{ProtocolVersion: mcpgo.ProtocolVersion20251125})
	// Transfer ownership only if initialization cancellation has not already
	// started closing the proxy, even if the SDK just completed its handshake.
	if !stopClosingProxy() && err == nil {
		err = initCtx.Err()
	}
	if err != nil {
		cancel()
		closeProxy()
		if session != nil {
			_ = session.Close()
		}
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
		// The SDK bounds its session termination request, which only reaches the
		// app while the lifetime context is live. The manager cancels that first,
		// so the app sees the connection drop instead. Close the proxy last to
		// join all of its streams.
		c.closeErr = c.session.Close()
		c.closeProxy()
		c.transport.CloseIdleConnections()
	})
	return c.closeErr
}
