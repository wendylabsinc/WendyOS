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

// appToolOpenTimeout bounds one attempt at bringing an app's MCP server up:
// the initialize handshake and the first tool listing. The streamable HTTP
// client has no timeout of its own, so without this a container whose port
// accepts TCP and then never answers wedges the single reconciler goroutine
// for the life of the session -- nothing new registered, nothing stopped
// removed, and a device switch leaving the previous device's tools in place.
const appToolOpenTimeout = 5 * time.Second

// appToolListTimeout bounds the container listing and any tool listing made
// through a proxy that is already open, for the same reason.
const appToolListTimeout = 5 * time.Second

// appToolOpenBudget caps how long one pass may spend opening new proxies. A
// device with several wedged apps would otherwise stretch a pass well past the
// rescan interval and delay every removal behind it; apps not reached in this
// pass are opened in the next.
const appToolOpenBudget = 15 * time.Second

// appToolRetryMin and appToolRetryMax bound the per-app backoff after a failed
// open, which doubles from the minimum. Retrying is the reconcile loop's job
// rather than a sleep inside a pass. The minimum is shorter than the rescan
// interval on purpose: a demo that was deployed a moment ago and is still
// starting its server must be picked up on the very next tick, which is what
// keeps "a new app appears within one interval" true for it. An app that is
// simply broken doubles away to the maximum and stops costing an attempt
// every pass.
const (
	appToolRetryMin = 2 * time.Second
	appToolRetryMax = time.Minute
)

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

// appRetryState holds an app back after a failed open, with the delay that
// produced it so the next failure can double it.
type appRetryState struct {
	next  time.Time
	delay time.Duration
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
	ticker := time.NewTicker(s.rescanEvery())
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
	// device being talked to. SetConn does this synchronously too; this covers
	// a pass that was in flight when the connection changed.
	if revision != s.appToolsRev {
		s.unregisterAllAppTools(srv)
		s.appToolsRev = revision
	}
	if conn == nil {
		return
	}

	listCtx, cancelList := context.WithTimeout(ctx, appToolListTimeout)
	apps, err := s.listMCPApps(listCtx, conn)
	cancelList()
	if err != nil {
		// Keep what is registered. A failed list is far likelier a blip than
		// every app vanishing at once, and tearing tools down on a blip is
		// worse than serving them one tick stale.
		return
	}

	prefixes, ambiguous := mcpToolPrefixes(apps)
	for _, name := range ambiguous {
		s.recordProxyDiag(name, "prefix-collision",
			fmt.Errorf("app id shares a sanitised tool prefix with another running app; its tools are not registered"))
	}

	for _, name := range s.registeredAppNames() {
		if _, ok := prefixes[name]; !ok {
			s.unregisterAppTools(srv, name)
		}
	}

	openUntil := time.Now().Add(appToolOpenBudget)
	for _, name := range apps {
		prefix, ok := prefixes[name]
		if !ok {
			continue // ambiguous prefix, already diagnosed
		}
		s.syncAppTools(ctx, srv, conn, name, prefix, revision, openUntil)
	}
}

// syncAppTools brings one app's registration up to date: opening a proxy for an
// app seen for the first time, and otherwise re-reading the tool list through
// the proxy already open for it.
func (s *mcpServer) syncAppTools(ctx context.Context, srv *server.MCPServer, conn *grpcclient.AgentConnection, appName, prefix string, revision uint64, openUntil time.Time) {
	set := s.appToolSet(appName)
	if set == nil {
		s.openAppTools(ctx, srv, conn, appName, prefix, revision, openUntil)
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, appToolListTimeout)
	result, err := set.client.ListTools(listCtx, mcpgo.ListToolsRequest{})
	cancel()
	if err != nil {
		// The app restarted, or its proxy died with it. A fresh proxy is the
		// only way back to a working client, so reopen in this same pass
		// rather than leaving the app toolless until the next one -- a restart
		// is the common case, not an exceptional one.
		s.recordProxyDiag(appName, "list-tools", err)
		s.unregisterAppTools(srv, appName)
		s.openAppTools(ctx, srv, conn, appName, prefix, revision, openUntil)
		return
	}

	signature := toolSignature(result.Tools)
	if signature == set.signature && prefix == set.prefix {
		return
	}
	// The proxy and the client are still good. A changed surface -- or a prefix
	// that changed because another app arrived and made the short form
	// ambiguous -- costs a re-registration and a tools/list_changed, not a
	// teardown and a fresh MCP handshake.
	srv.DeleteTools(set.toolNames...)
	s.storeAppTools(appName, &appToolSet{
		prefix:    prefix,
		toolNames: s.addProxiedTools(srv, set.client, appName, prefix, result.Tools),
		signature: signature,
		client:    set.client,
		close:     set.close,
	}, revision)
}

// openAppTools proxies one app's MCP server into srv with a single bounded
// attempt. Failures are isolated to the app, recorded in the
// wendy://diagnostics resource, and held back by a doubling backoff.
func (s *mcpServer) openAppTools(ctx context.Context, srv *server.MCPServer, conn *grpcclient.AgentConnection, appName, prefix string, revision uint64, openUntil time.Time) {
	if !s.appOpenDue(appName) || time.Now().After(openUntil) {
		return
	}

	addr, closeProxy, err := startMCPProxy(ctx, conn, appName)
	if err != nil {
		s.failAppTools(appName, "proxy", err)
		return
	}
	mcpCli, err := mcpclient.NewStreamableHttpClient("http://" + addr)
	if err != nil {
		closeAppTransport(nil, closeProxy)
		s.failAppTools(appName, "client", err)
		return
	}

	// One bounded attempt per pass. Sleeping through 2s+4s+8s of retries here
	// would hold the single reconciler goroutine while every other app waits,
	// and against an app that never answers it would hold it forever.
	openCtx, cancel := context.WithTimeout(ctx, appToolOpenTimeout)
	defer cancel()

	if _, err := mcpCli.Initialize(openCtx, mcpgo.InitializeRequest{}); err != nil {
		closeAppTransport(mcpCli, closeProxy)
		s.failAppTools(appName, "initialize", err)
		return
	}
	result, err := mcpCli.ListTools(openCtx, mcpgo.ListToolsRequest{})
	if err != nil {
		closeAppTransport(mcpCli, closeProxy)
		s.failAppTools(appName, "list-tools", err)
		return
	}

	if !s.storeAppTools(appName, &appToolSet{
		prefix:    prefix,
		toolNames: s.addProxiedTools(srv, mcpCli, appName, prefix, result.Tools),
		signature: toolSignature(result.Tools),
		client:    mcpCli,
		close:     closeProxy,
	}, revision) {
		// The connection changed while this app was being opened, so these
		// tools describe a device the client is no longer talking to.
		srv.DeleteTools(prefixedToolNames(prefix, result.Tools)...)
		closeAppTransport(mcpCli, closeProxy)
	}
}

// closeAppTransport tears down an app's client and proxy off the reconciler
// goroutine. Closing a streamable HTTP client sends a DELETE to end the MCP
// session, which against a device that has gone away blocks until its own
// timeout -- seconds, per app, in the middle of a pass.
func closeAppTransport(cli *mcpclient.Client, closeProxy func()) {
	go func() {
		if cli != nil {
			_ = cli.Close()
		}
		if closeProxy != nil {
			closeProxy()
		}
	}()
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

// prefixedToolNames is the name set addProxiedTools would register, used to
// undo a registration whose connection went stale underneath it.
func prefixedToolNames(prefix string, tools []mcpgo.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := prefix + mcpToolSeparator + tool.Name; len(name) <= maxMCPToolName {
			names = append(names, name)
		}
	}
	return names
}

// listMCPApps names every running app on the device that serves MCP, sorted so
// a pass is deterministic.
func (s *mcpServer) listMCPApps(ctx context.Context, conn *grpcclient.AgentConnection) ([]string, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		s.warnProxyDiag("", "list-containers", err)
		return nil, err
	}
	var names []string
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.warnProxyDiag("", "read-container-list", err)
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

// mcpToolPrefixes assigns every app its tool-name prefix, resolving collisions
// across the whole set rather than per app: two apps sharing a last segment
// ("a.status" and "b.status") both fall back to their full id, so neither
// silently answers for the other.
//
// The second return names the apps left without a prefix. sanitizeMCPPrefix is
// not injective -- "a.b-x" and "a.b_x" both become "a_b_x" -- so the full-id
// fallback is not guaranteed to separate two apps either. Whatever prefix is
// still shared is withheld from every app holding it: AddTool would silently
// overwrite one registration with the other and route calls to the wrong app,
// and a tool that is absent and diagnosed beats one that answers as an app the
// caller did not name.
func mcpToolPrefixes(appNames []string) (map[string]string, []string) {
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

	finalCount := make(map[string]int, len(prefixes))
	for _, prefix := range prefixes {
		finalCount[prefix]++
	}
	var ambiguous []string
	for name, prefix := range prefixes {
		if finalCount[prefix] > 1 {
			ambiguous = append(ambiguous, name)
			delete(prefixes, name)
		}
	}
	sort.Strings(ambiguous)
	return prefixes, ambiguous
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
// are read by tests and by SetConn's synchronous teardown, so both go through
// appMu. appMu is never held while s.mu is taken, or the other way round.

// rescanEvery is the reconcile interval, overridable by tests.
func (s *mcpServer) rescanEvery() time.Duration {
	if s.rescanInterval > 0 {
		return s.rescanInterval
	}
	return appToolRescanInterval
}

// retryFloor is the shortest backoff after a failed open, overridable by tests.
func (s *mcpServer) retryFloor() time.Duration {
	if s.retryBackoffMin > 0 {
		return s.retryBackoffMin
	}
	return appToolRetryMin
}

// toolServer returns the MCP server tools are registered on, or nil before
// Start has run.
func (s *mcpServer) toolServer() *server.MCPServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.srv
}

// connAndRevision reads the active connection together with the revision it was
// stored under, so a pass cannot mix a connection with another's revision.
func (s *mcpServer) connAndRevision() (*grpcclient.AgentConnection, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn, s.connRevision
}

// warnProxyDiag records a failure and, the first time that exact failure is
// seen, also warns on stderr. Repeats stay in the diagnostics count only: the
// reconciler retries for the life of the session, and a warning per pass would
// bury everything else in the host's log.
func (s *mcpServer) warnProxyDiag(appName, stage string, err error) {
	if s.recordProxyDiag(appName, stage, err) {
		if appName == "" {
			fmt.Fprintf(os.Stderr, "Warning: MCP %s: %v\n", stage, err)
			return
		}
		fmt.Fprintf(os.Stderr, "Warning: MCP %s for %s: %v\n", stage, appName, err)
	}
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

// storeAppTools records a successful registration and clears any backoff. It
// reports false, storing nothing, when the connection changed while the app was
// being opened -- the tools would describe a device the client has left.
func (s *mcpServer) storeAppTools(appName string, set *appToolSet, revision uint64) bool {
	if _, current := s.connAndRevision(); current != revision {
		return false
	}
	s.appMu.Lock()
	defer s.appMu.Unlock()
	if s.appTools == nil {
		s.appTools = make(map[string]*appToolSet)
	}
	s.appTools[appName] = set
	delete(s.appRetry, appName)
	return true
}

// failAppTools records why an app could not be proxied and holds it back, with
// the delay doubling on each consecutive failure.
func (s *mcpServer) failAppTools(appName, stage string, err error) {
	s.warnProxyDiag(appName, stage, err)

	s.appMu.Lock()
	defer s.appMu.Unlock()
	if s.appRetry == nil {
		s.appRetry = make(map[string]appRetryState)
	}
	state := s.appRetry[appName]
	state.delay *= 2
	if floor := s.retryFloor(); state.delay < floor {
		state.delay = floor
	}
	if state.delay > appToolRetryMax {
		state.delay = appToolRetryMax
	}
	state.next = time.Now().Add(state.delay)
	s.appRetry[appName] = state
}

// appOpenDue reports whether appName may be attempted in this pass.
func (s *mcpServer) appOpenDue(appName string) bool {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	state, held := s.appRetry[appName]
	return !held || !time.Now().Before(state.next)
}

// unregisterAppTools removes one app's tools. The names go synchronously so a
// tools/list straight afterwards is already correct; the transports close in
// the background because that can block for seconds against a device that has
// gone away.
func (s *mcpServer) unregisterAppTools(srv *server.MCPServer, appName string) {
	s.appMu.Lock()
	set := s.appTools[appName]
	delete(s.appTools, appName)
	s.appMu.Unlock()
	if set == nil {
		return
	}
	srv.DeleteTools(set.toolNames...)
	closeAppTransport(set.client, set.close)
}

// unregisterAllAppTools drops every proxied app, used on shutdown and whenever
// the connection changes device.
func (s *mcpServer) unregisterAllAppTools(srv *server.MCPServer) {
	if srv == nil {
		return
	}
	for _, name := range s.registeredAppNames() {
		s.unregisterAppTools(srv, name)
	}
	s.appMu.Lock()
	s.appRetry = nil
	s.appMu.Unlock()
}
