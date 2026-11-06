package backend

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type webAgent struct {
	pb.UnimplementedWendyContainerServiceServer
}

func (webAgent) ListContainers(_ *pb.ListContainersRequest, stream grpc.ServerStreamingServer[pb.ListContainersResponse]) error {
	for _, app := range []*pb.AppContainer{
		{AppName: "web", HttpPort: 8080, RunningState: pb.AppRunningState_RUNNING},
		{AppName: "stopped", HttpPort: 8080},
		{AppName: "no-http", RunningState: pb.AppRunningState_RUNNING},
		{AppName: "invalid-port", HttpPort: 65536, RunningState: pb.AppRunningState_RUNNING},
	} {
		if err := stream.Send(&pb.ListContainersResponse{Container: app}); err != nil {
			return err
		}
	}
	return nil
}
func webBackend(t *testing.T) *Backend {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterWendyContainerServiceServer(server, webAgent{})
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 3
	t.Cleanup(func() { b.Shutdown() })
	return b
}
func TestOpenAppLANAndEntitlementValidation(t *testing.T) {
	b := webBackend(t)
	for _, host := range []string{"device.local", "192.168.1.2", "2001:db8::1", "fe80::1%en0"} {
		b.conn.Host = host
		address, err := b.OpenApp("web", 3)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(address)
		if err != nil || u.Scheme != "http" || u.Hostname() != host || u.Port() != "8080" {
			t.Fatalf("URL = %q, %v", address, err)
		}
	}
	for _, app := range []string{"stopped", "no-http", "missing", "invalid-port"} {
		if _, err := b.OpenApp(app, 3); err == nil {
			t.Fatalf("opened %s", app)
		}
	}
	if _, err := b.OpenApp("web", 2); err == nil {
		t.Fatal("accepted stale session")
	}
}
func TestOpenAppTunnelReusesPortAndClosesWithSession(t *testing.T) {
	b := webBackend(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/page" || r.URL.RawQuery != "a=1" {
			t.Errorf("lost request URL: %v", r.URL)
		}
		io.WriteString(w, "device webpage")
	}))
	defer upstream.Close()
	b.conn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		if port != 8080 {
			t.Errorf("dialed undeclared port %d", port)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	}
	address, err := b.OpenApp("web", 3)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := b.OpenApp("web", 3); err != nil || again != address {
		t.Fatalf("did not reuse tunnel: %s %v", again, err)
	}
	u, _ := url.Parse(address)
	if u.Hostname() != "127.0.0.1" {
		t.Fatalf("listener is not loopback: %s", address)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(address + "page?a=1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "device webpage" {
		t.Fatalf("body = %q, %v", body, err)
	}
	idle, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if err := b.Disconnect(); err != nil {
		t.Fatal(err)
	}
	idle.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle browser connection survived disconnect")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("disconnect did not close idle connection")
	}
	if _, err := client.Get(address); err == nil {
		t.Fatal("listener survived device disconnect")
	}
}
