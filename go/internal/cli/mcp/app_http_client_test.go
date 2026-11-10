package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Exercise connectAppMCP itself, including the local TCP listener and StreamMCP,
// against the retained mcp-go HTTP server rather than an SDK-only fixture.
type httpAppAgent struct {
	agentpb.UnimplementedWendyContainerServiceServer
	address string
}

func (a *httpAppAgent) StreamMCP(stream grpc.BidiStreamingServer[agentpb.MCPChunk, agentpb.MCPChunk]) error {
	conn, err := (&net.Dialer{}).DialContext(stream.Context(), "tcp", a.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(stream.Context(), func() { _ = conn.Close() })
	defer stop()
	done := make(chan error, 1)
	go func() {
		for {
			chunk, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if _, err := conn.Write(chunk.Data); err != nil {
				done <- err
				return
			}
		}
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if err := stream.Send(&agentpb.MCPChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		select {
		case err := <-done:
			return err
		default:
		}
	}
}

func appHTTPFixture(t *testing.T, handler http.Handler) (*grpcclient.AgentConnection, context.Context, context.CancelFunc) {
	t.Helper()
	return appHTTPServerFixture(t, httptest.NewServer(handler))
}

func appHTTPServerFixture(t *testing.T, httpServer *httptest.Server) (*grpcclient.AgentConnection, context.Context, context.CancelFunc) {
	t.Helper()
	t.Cleanup(httpServer.Close)
	listener := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(srv, &httpAppAgent{address: httpServer.Listener.Addr().String()})
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { srv.Stop(); _ = listener.Close() })
	cc, err := grpc.NewClient("passthrough:///app", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return grpcclient.NewFromConn(cc), ctx, cancel
}

func connectHTTPFixture(t *testing.T, handler http.Handler) (appMCPClient, context.Context, context.CancelFunc) {
	t.Helper()
	conn, ctx, cancel := appHTTPFixture(t, handler)
	initCtx, initCancel := context.WithTimeout(ctx, 3*time.Second)
	defer initCancel()
	client, err := connectAppMCP(ctx, initCtx, conn, "test-app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, ctx, cancel
}

func TestAppHTTPInteroperability(t *testing.T) {
	protocol := server.NewMCPServer("app", "1", server.WithPaginationLimit(1))
	var tool mcpgo.Tool
	if err := json.Unmarshal([]byte(`{"name":"echo","description":"Echo input","inputSchema":{"type":"object","properties":{"value":{"oneOf":[{"type":"string"},{"type":"number"}]}},"required":["value"],"additionalProperties":false},"outputSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}},"annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"_meta":{"app":"test"}}`), &tool); err != nil {
		t.Fatal(err)
	}
	var want *mcpgo.CallToolResult
	rawResult := json.RawMessage(`{"content":[{"type":"text","text":"hello","annotations":{"audience":["user"],"priority":0.5}},{"type":"image","data":"aGk=","mimeType":"image/png"},{"type":"audio","data":"aGk=","mimeType":"audio/wav"},{"type":"resource","resource":{"uri":"file:///test","mimeType":"text/plain","text":"resource"}},{"type":"resource_link","uri":"file:///link","name":"link"}],"structuredContent":{"ok":true},"_meta":{"source":"app"},"isError":true}`)
	var err error
	want, err = mcpgo.ParseCallToolResult(&rawResult)
	if err != nil {
		t.Fatal(err)
	}
	protocol.AddTool(tool, func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !reflect.DeepEqual(req.GetArguments(), map[string]any{"value": "hello"}) {
			return nil, fmt.Errorf("unexpected arguments: %v", req.GetArguments())
		}
		return want, nil
	})
	for _, name := range []string{"another", "last"} {
		protocol.AddTool(mcpgo.NewTool(name), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		})
	}
	var pages atomic.Int32
	handler := server.NewStreamableHTTPServer(protocol)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Method string
				Params struct {
					ProtocolVersion string
					ClientInfo      mcpgo.Implementation
				}
			}
			_ = json.Unmarshal(body, &request)
			if request.Method == "initialize" && (request.Params.ProtocolVersion != mcpgo.ProtocolVersion20251125 || request.Params.ClientInfo.Name != "wendy-app-proxy") {
				t.Errorf("unexpected initialize: %s", body)
			}
			if request.Method == "tools/list" {
				pages.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	})
	client, ctx, _ := connectHTTPFixture(t, wrapped)
	tools, err := client.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 || pages.Load() != 4 || tools.NextCursor != "" {
		t.Fatalf("pagination lost tools: %+v, pages=%d", tools, pages.Load())
	}
	for _, got := range tools.Tools {
		if got.Name == "echo" {
			assertMCPJSON(t, tool, got)
		}
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = "echo", map[string]any{"value": "hello"}
	got, err := client.CallTool(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	assertMCPJSON(t, want, got)
	want.IsError = false
	got, err = client.CallTool(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	assertMCPJSON(t, want, got)
	req.Params.Name = "missing"
	if result, err := client.CallTool(ctx, req); err == nil || result != nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("protocol error became a tool result: %+v, %v", result, err)
	}
}

func assertMCPJSON(t *testing.T, want, got any) {
	t.Helper()
	var a, b any
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("MCP semantics changed:\nwant %v\ngot  %v", a, b)
	}
}

func TestAppHTTPRejectsRedirects(t *testing.T) {
	for _, stage := range []string{"initialize", "tools/list", "tools/call", "DELETE"} {
		for _, code := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", stage, code), func(t *testing.T) {
				var hits, redirected atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
				defer target.Close()
				protocol := server.NewMCPServer("app", "1")
				protocol.AddTool(mcpgo.NewTool("echo"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
					return mcpgo.NewToolResultText("ok"), nil
				})
				handler := server.NewStreamableHTTPServer(protocol)
				conn, ctx, _ := appHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					method := r.Method
					if method == "POST" {
						body, _ := io.ReadAll(r.Body)
						r.Body = io.NopCloser(bytes.NewReader(body))
						var req struct{ Method string }
						_ = json.Unmarshal(body, &req)
						method = req.Method
					}
					if method == stage {
						redirected.Add(1)
						http.Redirect(w, r, target.URL, code)
						return
					}
					handler.ServeHTTP(w, r)
				}))
				client, err := connectAppMCP(ctx, ctx, conn, "app")
				if stage == "initialize" {
					if err == nil {
						_ = client.Close()
						t.Fatal("redirected initialize succeeded")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if stage == "tools/list" {
						if _, err := client.ListTools(ctx, mcpgo.ListToolsRequest{}); err == nil {
							t.Fatal("redirected list succeeded")
						}
					}
					if stage == "tools/call" {
						req := mcpgo.CallToolRequest{}
						req.Params.Name = "echo"
						if _, err := client.CallTool(ctx, req); err == nil {
							t.Fatal("redirected call succeeded")
						}
					}
					_ = client.Close()
				}
				if redirected.Load() == 0 || hits.Load() != 0 {
					t.Fatalf("redirect attempts=%d, escaped requests=%d", redirected.Load(), hits.Load())
				}
			})
		}
	}
}

func TestAppHTTPCancellationAndClose(t *testing.T) {
	for _, action := range []string{"cancel-call", "cancel-lifetime", "close"} {
		t.Run(action, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			protocol := server.NewMCPServer("app", "1")
			protocol.AddTool(mcpgo.NewTool("wait"), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return nil, ctx.Err()
			})
			protocol.AddTool(mcpgo.NewTool("echo"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return mcpgo.NewToolResultText("ok"), nil
			})
			client, ctx, cancelLifetime := connectHTTPFixture(t, server.NewStreamableHTTPServer(protocol))
			callCtx, cancelCall := context.WithCancel(ctx)
			defer cancelCall()
			result := make(chan error, 1)
			go func() {
				req := mcpgo.CallToolRequest{}
				req.Params.Name = "wait"
				_, err := client.CallTool(callCtx, req)
				result <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("call did not start")
			}
			switch action {
			case "cancel-call":
				cancelCall()
			case "cancel-lifetime":
				cancelLifetime()
			case "close":
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("canceled call succeeded")
				}
				if action == "cancel-call" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("call did not stop")
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("server operation not canceled")
			}
			if action == "cancel-call" {
				req := mcpgo.CallToolRequest{}
				req.Params.Name = "echo"
				if _, err := client.CallTool(ctx, req); err != nil {
					t.Fatalf("cancellation broke session: %v", err)
				}
			}
			first := client.Close()
			if action != "cancel-lifetime" && first != nil {
				t.Fatal(first)
			}
			if second := client.Close(); second != first {
				t.Fatalf("close not idempotent: %v, %v", first, second)
			}
		})
	}
}

func TestAppHTTPListCancellationOnLaterPage(t *testing.T) {
	for _, action := range []string{"caller", "lifetime", "close"} {
		t.Run(action, func(t *testing.T) {
			protocol := server.NewMCPServer("app", "1", server.WithPaginationLimit(1))
			for _, name := range []string{"first", "second", "third"} {
				protocol.AddTool(mcpgo.NewTool(name), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
					return mcpgo.NewToolResultText("ok"), nil
				})
			}
			laterPage, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var pages atomic.Int32
			handler := server.NewStreamableHTTPServer(protocol)
			client, ctx, cancelLifetime := connectHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(body))
					var req struct {
						Method string
						Params struct{ Cursor string }
					}
					_ = json.Unmarshal(body, &req)
					if req.Method == "tools/list" {
						pages.Add(1)
						if req.Params.Cursor != "" {
							close(laterPage)
							select {
							case <-release:
							case <-r.Context().Done():
							}
							return
						}
					}
				}
				handler.ServeHTTP(w, r)
			}))
			listCtx, cancelList := context.WithCancel(ctx)
			defer cancelList()
			done := make(chan error, 1)
			go func() {
				result, err := client.ListTools(listCtx, mcpgo.ListToolsRequest{})
				if result != nil {
					done <- errors.New("returned a partial list")
					return
				}
				done <- err
			}()
			select {
			case <-laterPage:
			case <-ctx.Done():
				t.Fatal("did not request second page")
			}
			switch action {
			case "caller":
				cancelList()
			case "lifetime":
				cancelLifetime()
			case "close":
				_ = client.Close()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("later page did not cancel")
			}
			if pages.Load() != 2 {
				t.Fatalf("unexpected page count: %d", pages.Load())
			}
		})
	}
}

func TestAppHTTPFailedConnectCleansProxy(t *testing.T) {
	for _, mode := range []string{"error", "init-canceled", "lifetime-canceled"} {
		t.Run(mode, func(t *testing.T) {
			hosts := make(chan string, 1)
			release := make(chan struct{})
			defer close(release)
			conn, lifetimeCtx, cancelLifetime := appHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case hosts <- r.Host:
				default:
				}
				if mode != "error" {
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				w.WriteHeader(http.StatusInternalServerError)
			}))
			initCtx, cancelInit := context.WithCancel(context.Background())
			defer cancelInit()
			done := make(chan error, 1)
			go func() {
				client, err := connectAppMCP(lifetimeCtx, initCtx, conn, "app")
				if client != nil {
					_ = client.Close()
					done <- errors.New("failed connect returned client")
					return
				}
				done <- err
			}()
			var address string
			select {
			case address = <-hosts:
			case <-lifetimeCtx.Done():
				t.Fatal("initialize did not reach app")
			}
			if mode == "init-canceled" {
				cancelInit()
			}
			if mode == "lifetime-canceled" {
				cancelLifetime()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("connect succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("failed connect did not finish")
			}
			if c, err := net.DialTimeout("tcp", address, time.Second); err == nil {
				_ = c.Close()
				t.Fatal("failed initialization left proxy listener open")
			}
		})
	}
}

func TestAppHTTPFailedConnectCancellationNotification(t *testing.T) {
	// The SDK normally sends cancellation notifications asynchronously. If a
	// notification starts before failed Connect closes its session, graceful
	// cleanup waits for it. Its compatibility switch makes that ordering
	// deterministic; the existing test still enforces the same two-second
	// cancellation bound and verifies that the proxy listener is closed.
	// The SDK reads this setting at process initialization, so use a child
	// running only the failure/cleanup test, not the complete package suite.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestAppHTTPFailedConnectCleansProxy/(init-canceled|lifetime-canceled)$",
		"-test.count=1")
	cmd.Env = append(os.Environ(), "MCPGODEBUG=blockingcancelnotify=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialization cancellation waited for a notification: %v\n%s", err, output)
	}
}

func TestAppHTTPInitializationCancellationWinsSuccessfulHandshake(t *testing.T) {
	protocol := server.NewMCPServer("app", "1")
	conn, lifetimeCtx, _ := appHTTPFixture(t, server.NewStreamableHTTPServer(protocol))
	address, closeProxy, err := startMCPProxy(lifetimeCtx, conn, "app")
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy()
	initCtx, cancelInit := context.WithCancel(context.Background())
	defer cancelInit()
	closing, release := make(chan struct{}), make(chan struct{})
	releaseClose := sync.OnceFunc(func() { close(release) })
	defer releaseClose()
	var closes atomic.Int32
	var completed *sdk.ClientSession
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	client.AddSendingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
			result, err := next(ctx, method, request)
			if method == "notifications/initialized" && err == nil {
				completed = request.GetSession().(*sdk.ClientSession)
				cancelInit()
				// Finish a real successful SDK handshake only after the
				// constructor's cancellation callback has started closing.
				<-closing
			}
			return result, err
		}
	})
	type connected struct {
		client *appHTTPClient
		err    error
	}
	done := make(chan connected, 1)
	go func() {
		got, err := newAppHTTPClientWithSDK(lifetimeCtx, initCtx, "http://"+address, func() {
			close(closing)
			<-release
			closeProxy()
			closes.Add(1)
		}, client)
		done <- connected{got, err}
	}()
	select {
	case <-closing:
	case <-lifetimeCtx.Done():
		t.Fatal("initialization cancellation did not start proxy cleanup")
	}
	releaseClose()
	select {
	case got := <-done:
		if got.client != nil {
			_ = got.client.Close()
			t.Fatal("canceled initialization returned a client backed by a closing proxy")
		}
		if !errors.Is(got.err, context.Canceled) || closes.Load() != 1 {
			t.Fatalf("canceled initialization: err=%v proxy closes=%d", got.err, closes.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled successful handshake did not finish cleanup")
	}
	if c, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("canceled successful handshake left proxy listener open")
	}
	waited := make(chan struct{})
	go func() { _ = completed.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled successful handshake leaked its SDK session")
	}
}

func TestAppHTTPCloseBoundsSessionTermination(t *testing.T) {
	protocol := server.NewMCPServer("app", "1")
	handler := server.NewStreamableHTTPServer(protocol)
	deleting, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	client, _, _ := connectHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			close(deleting)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-deleting:
	case <-time.After(time.Second):
		t.Fatal("no session termination request")
	}
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("unresponsive app held close open")
	}
}

// The client has no consumer for server-initiated messages. A standalone GET
// would only let the app stall connect and kill the session by dropping it.
func TestAppHTTPOpensNoStandaloneStream(t *testing.T) {
	protocol := server.NewMCPServer("app", "1")
	protocol.AddTool(mcpgo.NewTool("echo"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	handler := server.NewStreamableHTTPServer(protocol)
	var gets atomic.Int32
	conn, ctx, _ := appHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	initCtx, cancelInit := context.WithTimeout(ctx, 2*time.Second)
	defer cancelInit()
	type connected struct {
		client appMCPClient
		err    error
	}
	done := make(chan connected, 1)
	go func() {
		client, err := connectAppMCP(ctx, initCtx, conn, "app")
		done <- connected{client, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.client.Close()
		result, err := got.client.ListTools(ctx, mcpgo.ListToolsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 1 {
			t.Fatalf("tools = %d, want 1", len(result.Tools))
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("connect outlived its 2s init deadline; GET requests: %d", gets.Load())
	}
	if n := gets.Load(); n != 0 {
		t.Fatalf("GET requests = %d, want 0", n)
	}
}

func TestAppHTTPClosesIdleConnections(t *testing.T) {
	original := appHTTPIdleConnTimeout
	appHTTPIdleConnTimeout = 50 * time.Millisecond
	t.Cleanup(func() { appHTTPIdleConnTimeout = original })

	var open atomic.Int32
	httpServer := httptest.NewUnstartedServer(server.NewStreamableHTTPServer(server.NewMCPServer("app", "1")))
	httpServer.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	httpServer.Start()
	conn, ctx, _ := appHTTPServerFixture(t, httpServer)
	client, err := connectAppMCP(ctx, ctx, conn, "app")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if open.Load() == 0 {
		t.Fatal("app saw no connection")
	}
	waitContainerMCP(t, func() bool { return open.Load() == 0 })
}

func TestAppHTTPConnectReportsAppResponse(t *testing.T) {
	conn, ctx, _ := appHTTPFixture(t, http.NotFoundHandler())
	client, err := connectAppMCP(ctx, ctx, conn, "app")
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("connect succeeded")
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("error = %q, want the app's Not Found response", err)
	}
}

func TestAppHTTPConnectReportsProxyFailure(t *testing.T) {
	httpServer := httptest.NewServer(http.NotFoundHandler())
	conn, ctx, _ := appHTTPServerFixture(t, httpServer)
	httpServer.Close() // The agent now fails to dial the app's MCP port.
	client, err := connectAppMCP(ctx, ctx, conn, "app")
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("connect succeeded")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %q, want the proxy's dial failure", err)
	}
}
