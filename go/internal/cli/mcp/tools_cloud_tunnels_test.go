package mcp

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestCloudTunnelManagementClosesListenerAndIsIdempotent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := New(&config.Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.cloudTunnels["owned"] = &mcpCloudTunnel{listener: ln, cancel: cancel, info: cloudTunnelInfo{ID: "owned", Device: "cloud://endpoint/org/1/asset/2", Protocol: "tcp", LocalAddr: ln.Addr().String(), RemotePort: 8080}}
	r, err := s.handleCloudTunnelList(context.Background(), callToolReq("cloud_tunnel_list", nil))
	if err != nil || r.IsError {
		t.Fatalf("list: %v %v", r, err)
	}
	rows := listPayload(t, r, "tunnels")
	if len(rows) != 1 || rows[0]["id"] != "owned" || rows[0]["device"] != "cloud://endpoint/org/1/asset/2" {
		t.Fatalf("lost identity: %+v", rows)
	}
	for i, want := range []string{"closed", "already_closed"} {
		r, err = s.handleCloudTunnelClose(context.Background(), callToolReq("cloud_tunnel_close", map[string]any{"id": "owned"}))
		if err != nil || r.IsError || structuredMap(t, r)["status"] != want {
			t.Fatalf("close %d: %v %v", i, r, err)
		}
	}
	if ctx.Err() == nil {
		t.Fatal("tunnel context still alive")
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("listener still accepts connections")
	}
	r, _ = s.handleCloudTunnelList(context.Background(), callToolReq("cloud_tunnel_list", nil))
	if rows := listPayload(t, r, "tunnels"); len(rows) != 0 {
		t.Fatalf("closed tunnel retained: %+v", rows)
	}
}

func TestCloudTunnelRequiresIntegerPortsBeforeResolvingDevice(t *testing.T) {
	s := New(&config.Config{}, nil)
	for _, args := range []map[string]any{nil, {"local_port": 1.5}, {"local_port": 1234, "remote_port": "80"}} {
		r, err := s.handleCloudTunnel(context.Background(), callToolReq("cloud_tunnel", args))
		if err != nil || !r.IsError || structuredMap(t, r)["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("invalid ports reached cloud lookup: %v %v", r, err)
		}
	}
}

func TestTunnelCancellationClosesIdleConnections(t *testing.T) {
	client, accepted := net.Pipe()
	remote, relay := net.Pipe()
	t.Cleanup(func() { client.Close(); accepted.Close(); remote.Close(); relay.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		mcpServeTunnelConn(ctx, accepted, func(context.Context) (net.Conn, error) { close(ready); return relay, nil })
	}()
	<-ready
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle relay survived cancellation")
	}
	for _, conn := range []net.Conn{client, remote} {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("peer not closed: %v", err)
		}
	}
}

func TestTunnelCleanupDoesNotRemoveReplacement(t *testing.T) {
	s := New(&config.Config{}, nil)
	oldCtx, oldCancel := context.WithCancel(context.Background())
	newCtx, newCancel := context.WithCancel(context.Background())
	t.Cleanup(oldCancel)
	t.Cleanup(newCancel)
	old, next := &mcpCloudTunnel{cancel: oldCancel}, &mcpCloudTunnel{cancel: newCancel}
	s.cloudTunnels["same"] = next
	s.removeCloudTunnel("same", old)
	if s.cloudTunnels["same"] != next || newCtx.Err() != nil || oldCtx.Err() == nil {
		t.Fatal("cleanup affected replacement")
	}
	s.closeCloudTunnels()
	if newCtx.Err() == nil || len(s.cloudTunnels) != 0 {
		t.Fatal("shutdown leaked tunnel")
	}
	lateCtx, lateCancel := context.WithCancel(context.Background())
	t.Cleanup(lateCancel)
	if s.addCloudTunnel("late", &mcpCloudTunnel{cancel: lateCancel}) || lateCtx.Err() == nil || len(s.cloudTunnels) != 0 {
		t.Fatal("in-flight creation leaked a tunnel after shutdown")
	}
}
