package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// appToolRescanInterval is how often the device's container list is re-read.
// An assistant session outlives many `wendy run`s, so a demo deployed,
// restarted or stopped after the session began has to reach tools/list without
// the client reconnecting. The list is a cheap streaming RPC; at this interval
// a redeploy is invisible for at most one tick.
const appToolRescanInterval = 10 * time.Second

// appToolRetryBackoff is how long an app that failed to proxy is left alone.
// Opening a proxy costs up to ~14s of initialize retries, so without a backoff
// one permanently broken app would spend longer failing than the rescan
// interval and starve every healthy app behind it in the same pass.
const appToolRetryBackoff = time.Minute

// maxMCPToolName is the tool-name budget MCP clients commonly enforce. A name
// over it is skipped and recorded, never truncated: a silently shortened name
// can collide with another app's, and a collision routes a call to the wrong
// app -- worse than the tool being absent and diagnosable.
const maxMCPToolName = 64

// mcpToolSeparator joins an app's prefix to the app's own tool name.
const mcpToolSeparator = "__"

// appToolSet is one proxied app as currently registered: the live client and
// proxy behind it, the exact tool names added to the server so they can be
// removed again, and the inputs that decide whether the registration is still
// correct.
type appToolSet struct {
	prefix    string
	toolNames []string
	signature string
	client    *mcpclient.Client
	close     func()
}

// runAppToolReconciler keeps proxied app tools in step with the device for as
// long as the server runs.
//
// Registration used to happen exactly once, from the startup connect, which
// made two ordinary things quietly broken: an app deployed after the client
// started never appeared, and a client that started bare and then called
// device_connect got a connection with no app tools at all. Both are the same
// one-shot-scan bug, so both are fixed by reconciling rather than registering.
func (s *mcpServer) runAppToolReconciler(ctx context.Context, srv *server.MCPServer) {
	ticker := time.NewTicker(appToolRescanInterval)
	defer ticker.Stop()
	// Proxies left open past shutdown strand the TCP listeners the agent
	// relays through.
	defer s.unregisterAllAppTools(srv)

	for {
		s.reconcileAppTools(ctx, srv)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.reconcileCh:
		}
	}
}

// triggerAppToolReconcile asks for a pass now rather than at the next tick.
// Non-blocking and coalescing: the channel holds one pending wake-up and a
// second trigger before it is drained is already represented. Called after any
// connection change so device_connect's tools are present by the time the
// caller looks, instead of up to one interval later.
func (s *mcpServer) triggerAppToolReconcile() {
	select {
	case s.reconcileCh <- struct{}{}:
	default:
	}
}

// reconcileAppTools makes one pass. It runs only on the reconciler goroutine.
func (s *mcpServer) reconcileAppTools(ctx context.Context, srv *server.MCPServer) {
	conn, revision := s.connAndRevision()

	// A new connection is a different device, or none. Tools registered for the
	// previous one name apps this client can no longer reach, so they go before
	// anything else is considered -- the tool list must always describe the
	// device being talked to.
	if revision != s.appToolsRev {
		s.unregisterAllAppTools(srv)
		s.appToolsRev = revision
	}
	if conn == nil {
		return
	}

	apps, err := s.listMCPApps(ctx, conn)
	if err != nil {
		// Keep what is registered. A failed list is far likelier a blip than
		// every app vanishing at once, and tearing tools down on a blip is
		// worse than serving them one tick stale.
		return
	}
	prefixes := mcpToolPrefixes(apps)

	for _, name := range s.registeredAppNames() {
		if _, ok := prefixes[name]; !ok {
			s.unregisterAppTools(srv, name)
		}
	}
	for _, name := range apps {
		s.syncAppTools(ctx, srv, conn, name, prefixes[name])
	}
}

// syncAppTools brings one app's registration up to date: opening a proxy for an
// app seen for the first time, and otherwise re-reading the tool list through
// the proxy already open for it.
func (s *mcpServer) syncAppTools(ctx context.Context, srv *server.MCPServer, conn *grpcclient.AgentConnection, appName, prefix string) {
	set := s.appToolSet(appName)
	if set == nil {
		s.openAppTools(ctx, srv, conn, appName, prefix)
		return
	}
	// A prefix changes when another app arrives or leaves and the short form
	// stops (or starts) being unique. The registered names are then wrong.
	if set.prefix != prefix {
		s.unregisterAppTools(srv, appName)
		s.openAppTools(ctx, srv, conn, appName, prefix)
		return
	}

	result, err := set.client.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		// The app restarted, or its proxy died with it. A fresh proxy is the
		// only way back to a working client, so reopen in this same pass
		// rather than leaving the app toolless until the next one -- a restart
		// is the common case, not an exceptional one.
		s.recordProxyDiag(appName, "list-tools", err)
		s.unregisterAppTools(srv, appName)
		s.openAppTools(ctx, srv, conn, appName, prefix)
		return
	}
	if toolSignature(result.Tools) == set.signature {
		return
	}
	// Same app, different surface. Replace the names wholesale rather than
	// diffing them, so a renamed or removed tool cannot linger.
	srv.DeleteTools(set.toolNames...)
	s.storeAppTools(appName, &appToolSet{
		prefix:    prefix,
		toolNames: s.addProxiedTools(srv, set.client, appName, prefix, result.Tools),
		signature: toolSignature(result.Tools),
		client:    set.client,
		close:     set.close,
	})
}

// openAppTools proxies one app's MCP server into srv. Initialize is retried up
// to 4 times with exponential backoff (2s, 4s, 8s) because an app that was just
// deployed may still be starting its server. Failures are isolated to the app
// and recorded in the wendy://diagnostics resource.
func (s *mcpServer) openAppTools(ctx context.Context, srv *server.MCPServer, conn *grpcclient.AgentConnection, appName, prefix string) {
	if until, ok := s.appRetryAt(appName); ok && time.Now().Before(until) {
		return
	}

	addr, closeProxy, err := startMCPProxy(ctx, conn, appName)
	if err != nil {
		s.failAppTools(appName, "proxy", err)
		return
	}

	mcpCli, err := mcpclient.NewStreamableHttpClient("http://" + addr)
	if err != nil {
		closeProxy()
		s.failAppTools(appName, "client", err)
		return
	}

	var initErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				closeProxy()
				return
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		_, initErr = mcpCli.Initialize(ctx, mcpgo.InitializeRequest{})
		if initErr == nil {
			break
		}
	}
	if initErr != nil {
		closeProxy()
		s.failAppTools(appName, "initialize", initErr)
		return
	}

	result, err := mcpCli.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		closeProxy()
		s.failAppTools(appName, "list-tools", err)
		return
	}

	s.storeAppTools(appName, &appToolSet{
		prefix:    prefix,
		toolNames: s.addProxiedTools(srv, mcpCli, appName, prefix, result.Tools),
		signature: toolSignature(result.Tools),
		client:    mcpCli,
		close:     closeProxy,
	})
}

// addProxiedTools registers one app's tools under prefix and returns the names
// actually added, which is what unregistering later removes.
func (s *mcpServer) addProxiedTools(srv *server.MCPServer, cli *mcpclient.Client, appName, prefix string, tools []mcpgo.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		proxied := tool
		proxied.Name = prefix + mcpToolSeparator + tool.Name
		if len(proxied.Name) > maxMCPToolName {
			s.recordProxyDiag(appName, "tool-name-too-long",
				fmt.Errorf("%q is %d characters, over the %d-character MCP limit; tool skipped",
					proxied.Name, len(proxied.Name), maxMCPToolName))
			continue
		}
		originalName := tool.Name
		srv.AddTool(proxied, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			inner := mcpgo.CallToolRequest{}
			inner.Params.Name = originalName
			inner.Params.Arguments = req.Params.Arguments
			result, err := cli.CallTool(ctx, inner)
			if err != nil {
				return result, err
			}
			// Container-supplied tools are not held to the same output
			// discipline as wendy's own tools; cap the result the same way
			// okResultBounded/okTextBounded cap native ones (see results.go).
			return capProxiedResult(result, defaultProxyMaxBytes), nil
		})
		names = append(names, proxied.Name)
	}
	return names
}

// listMCPApps names every running app on the device that serves MCP, sorted so
// a pass is deterministic.
func (s *mcpServer) listMCPApps(ctx context.Context, conn *grpcclient.AgentConnection) ([]string, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		s.recordProxyDiag("", "list-containers", err)
		fmt.Fprintf(os.Stderr, "Warning: listing containers for MCP tools: %v\n", err)
		return nil, err
	}
	var names []string
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.recordProxyDiag("", "read-container-list", err)
			fmt.Fprintf(os.Stderr, "Warning: reading container list: %v\n", err)
			return nil, err
		}
		c := resp.GetContainer()
		if c == nil || c.GetMcpPort() == 0 || c.GetRunningState() != agentpb.AppRunningState_RUNNING {
			continue
		}
		names = append(names, c.GetAppName())
	}
	sort.Strings(names)
	return names, nil
}

// shortMCPPrefix is the last dot-separated segment of an app id, sanitised. App
// ids are reverse-DNS ("sh.wendy.demos.go2-patrol"), so a full id spends most
// of the 64-character tool-name budget on a domain every app on the device
// shares; the last segment is what actually names the app.
func shortMCPPrefix(appName string) string {
	segment := appName
	if i := strings.LastIndex(appName, "."); i >= 0 && i+1 < len(appName) {
		segment = appName[i+1:]
	}
	if segment == "" {
		return sanitizeMCPPrefix(appName)
	}
	return sanitizeMCPPrefix(segment)
}

// mcpToolPrefixes assigns every app its prefix, resolving collisions across the
// whole set rather than per app: two apps sharing a last segment ("a.status"
// and "b.status") both fall back to their full id, so neither silently answers
// for the other.
func mcpToolPrefixes(appNames []string) map[string]string {
	shortCount := make(map[string]int, len(appNames))
	for _, name := range appNames {
		shortCount[shortMCPPrefix(name)]++
	}
	prefixes := make(map[string]string, len(appNames))
	for _, name := range appNames {
		prefix := shortMCPPrefix(name)
		if shortCount[prefix] > 1 {
			prefix = sanitizeMCPPrefix(name)
		}
		prefixes[name] = prefix
	}
	return prefixes
}

// toolSignature summarises an app's tool list so a changed surface is detected
// without keeping the previous definitions. Descriptions count as well as
// names: a redeployed demo often keeps its names and changes what they say.
func toolSignature(tools []mcpgo.Tool) string {
	entries := make([]string, 0, len(tools))
	for _, t := range tools {
		entries = append(entries, t.Name+"\x00"+t.Description)
	}
	sort.Strings(entries)
	return strings.Join(entries, "\x1f")
}

// sanitizeMCPPrefix converts an app name to a valid MCP tool name prefix
// by replacing non-alphanumeric characters with underscores.
func sanitizeMCPPrefix(appName string) string {
	b := make([]byte, len(appName))
	for i := range appName {
		c := appName[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b[i] = c
		} else {
			b[i] = '_'
		}
	}
	return string(b)
}

// --- registration state -----------------------------------------------------
//
// appTools and appRetry are written only by the reconciler goroutine, but they
// are read by tests and could be read by a future diagnostics surface, so both
// go through appMu rather than relying on that invariant holding forever.

// connAndRevision reads the active connection together with the revision it was
// stored under, so a pass cannot mix a connection with another's revision.
func (s *mcpServer) connAndRevision() (*grpcclient.AgentConnection, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn, s.connRevision
}

// appToolSet returns the registration for appName, or nil if it has none.
func (s *mcpServer) appToolSet(appName string) *appToolSet {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	return s.appTools[appName]
}

// registeredAppNames lists the apps with tools currently registered, sorted.
func (s *mcpServer) registeredAppNames() []string {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	names := make([]string, 0, len(s.appTools))
	for name := range s.appTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// storeAppTools records a successful registration and clears any retry backoff.
func (s *mcpServer) storeAppTools(appName string, set *appToolSet) {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	if s.appTools == nil {
		s.appTools = make(map[string]*appToolSet)
	}
	s.appTools[appName] = set
	delete(s.appRetry, appName)
}

// failAppTools records why an app could not be proxied and holds it back from
// the next few passes.
func (s *mcpServer) failAppTools(appName, stage string, err error) {
	s.recordProxyDiag(appName, stage, err)
	fmt.Fprintf(os.Stderr, "Warning: MCP %s for %s: %v\n", stage, appName, err)
	s.appMu.Lock()
	defer s.appMu.Unlock()
	if s.appRetry == nil {
		s.appRetry = make(map[string]time.Time)
	}
	s.appRetry[appName] = time.Now().Add(appToolRetryBackoff)
}

// appRetryAt reports when appName may be retried, if it is being held back.
func (s *mcpServer) appRetryAt(appName string) (time.Time, bool) {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	at, ok := s.appRetry[appName]
	return at, ok
}

// unregisterAppTools removes one app's tools and closes its proxy.
func (s *mcpServer) unregisterAppTools(srv *server.MCPServer, appName string) {
	s.appMu.Lock()
	set := s.appTools[appName]
	delete(s.appTools, appName)
	s.appMu.Unlock()
	if set == nil {
		return
	}
	// Removing the names before closing the transport keeps the window where a
	// client can call a tool whose proxy is already gone as short as possible.
	srv.DeleteTools(set.toolNames...)
	if set.client != nil {
		_ = set.client.Close()
	}
	if set.close != nil {
		set.close()
	}
}

// unregisterAllAppTools drops every proxied app, used on shutdown and whenever
// the connection changes device.
func (s *mcpServer) unregisterAllAppTools(srv *server.MCPServer) {
	for _, name := range s.registeredAppNames() {
		s.unregisterAppTools(srv, name)
	}
	s.appMu.Lock()
	s.appRetry = nil
	s.appMu.Unlock()
}
