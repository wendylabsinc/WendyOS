package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestStart_ServesWhileStartupConnectIsBlocked(t *testing.T) {
	originalServeStdio := serveStdio
	t.Cleanup(func() { serveStdio = originalServeStdio })

	serveStarted := make(chan map[string]*server.ServerTool, 1)
	allowServeReturn := make(chan struct{})
	serveStdio = func(srv *server.MCPServer) error {
		serveStarted <- srv.ListTools()
		<-allowServeReturn
		return nil
	}

	connectStarted := make(chan struct{})
	connectCanceled := make(chan struct{})
	s := New(&config.Config{}, nil)
	s.SetStartupConnect(func(ctx context.Context) {
		close(connectStarted)
		<-ctx.Done()
		close(connectCanceled)
	})

	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()

	select {
	case tools := <-serveStarted:
		if _, ok := tools["wendy_status"]; !ok {
			t.Fatal("stdio server started without built-in Wendy tools")
		}
	case <-time.After(time.Second):
		t.Fatal("stdio server did not start while startup connection was blocked")
	}

	select {
	case <-connectStarted:
	case <-time.After(time.Second):
		t.Fatal("startup connection did not begin")
	}

	close(allowServeReturn)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after stdio server stopped")
	}

	select {
	case <-connectCanceled:
	case <-time.After(time.Second):
		t.Fatal("startup connection context was not canceled when stdio stopped")
	}
}

func TestConnectToOnStartup_DoesNotOverwriteExplicitConnection(t *testing.T) {
	connectStarted := make(chan struct{})
	allowConnect := make(chan struct{})
	startupConn := &grpcclient.AgentConnection{}
	explicitConn := &grpcclient.AgentConnection{}
	s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		close(connectStarted)
		<-allowConnect
		return startupConn, nil
	})

	done := make(chan error, 1)
	go func() { done <- s.ConnectToOnStartup(context.Background(), "default.local:50051") }()
	<-connectStarted

	// This models device_connect/cloud_connect winning while the automatic
	// default-device attempt is still pending.
	s.SetConn(explicitConn)
	close(allowConnect)
	if err := <-done; err != nil {
		t.Fatalf("ConnectToOnStartup returned error: %v", err)
	}
	if got := s.GetConn(); got != explicitConn {
		t.Fatalf("startup connection overwrote explicit connection: got %p, want %p", got, explicitConn)
	}
}

func TestNew_NotNil(t *testing.T) {
	srv := New(&config.Config{}, nil)
	if srv == nil {
		t.Fatal("New returned nil")
	}
}

func TestGetConn_NilBeforeConnect(t *testing.T) {
	srv := New(&config.Config{}, nil)
	if srv.GetConn() != nil {
		t.Fatal("expected nil connection before connect")
	}
}

func TestGuideResource_ReturnsText(t *testing.T) {
	srv := New(&config.Config{}, nil)
	contents, err := srv.handleGuideResource(context.Background(), mcpgo.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(contents))
	}
	tc, ok := contents[0].(mcpgo.TextResourceContents)
	if !ok {
		t.Fatalf("expected TextResourceContents, got %T", contents[0])
	}
	if tc.URI != "wendy://guide" {
		t.Errorf("expected URI wendy://guide, got %q", tc.URI)
	}
	if tc.MIMEType != "text/plain" {
		t.Errorf("expected MIME text/plain, got %q", tc.MIMEType)
	}
	if len(tc.Text) < 100 {
		t.Errorf("expected guide text to be at least 100 chars, got %d", len(tc.Text))
	}
	if !strings.Contains(tc.Text, "error_code") {
		t.Errorf("expected guide text to mention error_code, got %q", tc.Text)
	}
}

// TestDeadTools_NotRegistered locks in the removal of dead/duplicate tools:
// it registers the same tool set Start() does and asserts each is absent
// from the server's tool list. filesync_sync was removed as unused; cloud_run
// and cloud_device_connect were removed as pure aliases of run and
// cloud_connect (same handler, same schema) that only added tool-selection
// noise for callers.
func TestDeadTools_NotRegistered(t *testing.T) {
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
	s.registerStatusTools(srv)
	s.registerDeviceTools(srv)
	s.registerContainerTools(srv)
	s.registerTelemetryTools(srv)
	s.registerWiFiTools(srv)
	s.registerBluetoothTools(srv)
	s.registerHardwareTools(srv)
	s.registerCameraTools(srv)
	s.registerProvisioningTools(srv)
	s.registerOSTools(srv)
	s.registerCloudTools(srv)
	s.registerContainerMCPTools(context.Background(), srv) // no active connection; no-op

	tools := srv.ListTools()
	for _, name := range []string{"filesync_sync", "cloud_run", "cloud_device_connect"} {
		if _, ok := tools[name]; ok {
			t.Fatalf("%s should not be registered", name)
		}
	}
}
