package mcp

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	containerMCPRefreshInterval  = 5 * time.Second
	containerMCPDiscoveryTimeout = 5 * time.Second
)

// appMCPClient is deliberately small so lifecycle tests can exercise connection
// changes without starting an HTTP server for every app.
type appMCPClient interface {
	ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error)
	CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	Close() error
}

type containerMCPApp struct {
	client      appMCPClient
	ctx         context.Context
	cancel      context.CancelFunc
	version     string
	port        uint32
	failures    uint32
	tools       map[string]mcpgo.Tool
	cancelTools context.CancelFunc
}

// The run goroutine owns app clients. Fields below marked guarded by s.mu are
// also used by SetConn to invalidate descriptors and cancel old calls immediately.
type containerMCPManager struct {
	s                *mcpServer
	srv              *server.MCPServer
	wake             chan struct{}
	interval         time.Duration
	timeout          time.Duration
	listApps         func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error)
	connectApp       func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error)
	generationCancel context.CancelFunc          // guarded by s.mu
	registered       map[string]*containerMCPApp // guarded by s.mu
}

func newContainerMCPManager(s *mcpServer, srv *server.MCPServer) *containerMCPManager {
	return &containerMCPManager{
		s: s, srv: srv, wake: make(chan struct{}, 1),
		interval: containerMCPRefreshInterval, timeout: containerMCPDiscoveryTimeout,
		listApps: listContainerMCPApps, connectApp: connectAppMCP,
		registered: make(map[string]*containerMCPApp),
	}
}

func (s *mcpServer) startContainerMCP(ctx context.Context, srv *server.MCPServer) func() {
	m := newContainerMCPManager(s, srv)
	return s.startContainerMCPManager(ctx, m)
}

func (s *mcpServer) startContainerMCPManager(ctx context.Context, m *containerMCPManager) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.mu.Lock()
	s.containerMCP = m
	s.mu.Unlock()
	go func() {
		defer close(done)
		m.run(ctx)
	}()
	return func() {
		cancel()
		<-done
		s.mu.Lock()
		if s.containerMCP == m {
			s.containerMCP = nil
		}
		s.mu.Unlock()
	}
}

// refreshContainerMCPTools schedules discovery without holding up a tool response.
// The periodic scan also finds apps deployed through another CLI or MCP session.
func (s *mcpServer) refreshContainerMCPTools() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.containerMCP != nil {
		s.containerMCP.signal()
	}
}

func (m *containerMCPManager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// invalidateLocked requires s.mu. Tool handlers also inspect their canceled
// context so a previously fetched descriptor cannot execute against the old app.
func (m *containerMCPManager) invalidateLocked() {
	if m.generationCancel != nil {
		m.generationCancel()
	}
	names := make([]string, 0, len(m.registered))
	for name := range m.registered {
		names = append(names, name)
	}
	m.srv.DeleteTools(names...)
	clear(m.registered)
	m.signal()
}

func (m *containerMCPManager) run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	apps := make(map[string]*containerMCPApp)
	var generationCtx context.Context
	var revision uint64
	defer func() {
		m.s.mu.Lock()
		m.invalidateLocked()
		m.s.mu.Unlock()
		for _, app := range apps {
			app.cancel()
			_ = app.client.Close()
		}
	}()
	for {
		m.s.mu.Lock()
		conn := m.s.conn
		changed := generationCtx == nil || revision != m.s.connRevision
		if changed {
			revision = m.s.connRevision
			generationCtx, m.generationCancel = context.WithCancel(ctx)
		}
		m.s.mu.Unlock()
		if changed {
			for name, app := range apps {
				m.removeApp(app)
				delete(apps, name)
			}
		}
		if conn != nil && generationCtx.Err() == nil {
			m.reconcile(generationCtx, conn, revision, apps)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

func (m *containerMCPManager) reconcile(ctx context.Context, conn *grpcclient.AgentConnection, revision uint64, apps map[string]*containerMCPApp) {
	listCtx, cancel := context.WithTimeout(ctx, m.timeout)
	containers, err := m.listApps(listCtx, conn)
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		m.s.recordProxyDiag("", "list-containers", err)
		// Availability is unknown; do not leave callable stale descriptors.
		for name, app := range apps {
			m.removeApp(app)
			delete(apps, name)
		}
		return
	}
	wanted := make(map[string]*agentpb.AppContainer)
	prefixes := make(map[string]int)
	for _, c := range containers {
		if c.GetAppName() != "" && c.GetMcpPort() > 0 && c.GetRunningState() == agentpb.AppRunningState_RUNNING {
			wanted[c.GetAppName()] = c
		}
	}
	for name := range wanted {
		prefixes[sanitizeMCPPrefix(name)]++
	}
	for name := range wanted {
		if prefixes[sanitizeMCPPrefix(name)] > 1 {
			m.s.recordProxyDiag(name, "tool-prefix", fmt.Errorf("app name has the same MCP prefix as another running app"))
			delete(wanted, name)
		}
	}
	for name, app := range apps {
		c := wanted[name]
		if c == nil || c.GetAppVersion() != app.version || c.GetMcpPort() != app.port || c.GetFailureCount() != app.failures {
			m.removeApp(app)
			delete(apps, name)
		}
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		app := apps[name]
		if app == nil {
			appCtx, appCancel := context.WithCancel(ctx)
			initCtx, initCancel := context.WithTimeout(appCtx, m.timeout)
			client, err := m.connectApp(appCtx, initCtx, conn, name)
			initCancel()
			if err != nil {
				appCancel()
				m.s.recordProxyDiag(name, "connect", err)
				continue
			}
			c := wanted[name]
			app = &containerMCPApp{client: client, ctx: appCtx, cancel: appCancel, version: c.GetAppVersion(), port: c.GetMcpPort(), failures: c.GetFailureCount()}
			apps[name] = app
		}
		// Client.ListTools follows all pagination cursors within this deadline.
		toolsCtx, toolsCancel := context.WithTimeout(app.ctx, m.timeout)
		result, err := app.client.ListTools(toolsCtx, mcpgo.ListToolsRequest{})
		toolsCancel()
		if err != nil {
			m.s.recordProxyDiag(name, "list-tools", err)
			m.removeApp(app)
			delete(apps, name)
			continue
		}
		if result != nil {
			m.publish(ctx, revision, name, app, result.Tools)
		}
	}
}

func (m *containerMCPManager) removeApp(app *containerMCPApp) {
	app.cancel()
	m.s.mu.Lock()
	var names []string
	for name, owner := range m.registered {
		if owner == app {
			names = append(names, name)
			delete(m.registered, name)
		}
	}
	m.srv.DeleteTools(names...)
	m.s.mu.Unlock()
	_ = app.client.Close()
}

func (m *containerMCPManager) publish(ctx context.Context, revision uint64, appName string, app *containerMCPApp, tools []mcpgo.Tool) {
	prefixed := make(map[string]mcpgo.Tool, len(tools))
	originals := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := sanitizeMCPPrefix(appName) + "__" + tool.Name
		originals[name] = tool.Name
		tool.Name = name
		prefixed[name] = tool
	}
	if reflect.DeepEqual(app.tools, prefixed) {
		return
	}
	m.s.mu.Lock()
	var collision string
	defer func() {
		m.s.mu.Unlock()
		if collision != "" {
			m.s.recordProxyDiag(appName, "tool-name", fmt.Errorf("MCP tool %q is already registered by another app or Wendy", collision))
		}
	}()
	if ctx.Err() != nil || m.s.connRevision != revision {
		return
	}
	if app.cancelTools != nil {
		app.cancelTools()
	}
	toolsCtx, cancelTools := context.WithCancel(app.ctx)
	app.cancelTools = cancelTools
	var oldNames []string
	for name, owner := range m.registered {
		if owner == app {
			oldNames = append(oldNames, name)
			delete(m.registered, name)
		}
	}
	// Delete before replacement also invalidates mcp-go's cached input schemas.
	m.srv.DeleteTools(oldNames...)
	for name := range prefixed {
		if m.srv.GetTool(name) != nil {
			// Keep the existing destination. Retry this publication on later
			// scans so it can recover when the conflicting owner disappears.
			collision = name
			app.tools = nil
			return
		}
	}
	entries := make([]server.ServerTool, 0, len(prefixed))
	for name, tool := range prefixed {
		original := originals[name]
		entries = append(entries, server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if toolsCtx.Err() != nil || !m.s.isConnRevision(revision) {
				return errResult(errCodeNotConnected, "app tools changed — refresh the tool list before retrying"), nil
			}
			callCtx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(toolsCtx, cancel)
			defer stop()
			defer cancel()
			inner := mcpgo.CallToolRequest{}
			inner.Params.Name = original
			inner.Params.Arguments = req.Params.Arguments
			result, err := app.client.CallTool(callCtx, inner)
			if toolsCtx.Err() != nil || !m.s.isConnRevision(revision) {
				return errResult(errCodeNotConnected, "device or app changed during the call; its result is no longer current"), nil
			}
			if err != nil {
				m.signal()
				return result, err
			}
			return capProxiedResult(result, defaultProxyMaxBytes), nil
		}})
		m.registered[name] = app
	}
	if len(entries) > 0 {
		m.srv.AddTools(entries...)
	}
	app.tools = prefixed
}

func (s *mcpServer) isConnRevision(revision uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connRevision == revision
}

func listContainerMCPApps(ctx context.Context, conn *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	var apps []*agentpb.AppContainer
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return apps, nil
		}
		if err != nil {
			return nil, err
		}
		apps = append(apps, resp.GetContainer())
	}
}

type proxiedAppMCPClient struct {
	*mcpclient.Client
	closeProxy func()
}

func (c *proxiedAppMCPClient) Close() error {
	c.closeProxy()
	return c.Client.Close()
}

func connectAppMCP(lifetimeCtx, initCtx context.Context, conn *grpcclient.AgentConnection, appName string) (appMCPClient, error) {
	addr, closeProxy, err := startMCPProxy(lifetimeCtx, conn, appName)
	if err != nil {
		return nil, err
	}
	client, err := mcpclient.NewStreamableHttpClient("http://" + addr)
	if err != nil {
		closeProxy()
		return nil, err
	}
	proxied := &proxiedAppMCPClient{Client: client, closeProxy: closeProxy}
	if _, err := client.Initialize(initCtx, mcpgo.InitializeRequest{}); err != nil {
		_ = proxied.Close()
		return nil, err
	}
	return proxied, nil
}
