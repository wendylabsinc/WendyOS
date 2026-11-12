package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type registryLifetimeAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	entered chan struct{}
	stopped chan struct{}
}

func (a *registryLifetimeAgent) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	if len(metadata.ValueFromIncomingContext(ctx, "regression-hold")) != 0 {
		close(a.entered)
		<-ctx.Done()
		close(a.stopped)
		return nil, ctx.Err()
	}
	return &agentpb.GetAgentVersionResponse{Version: "registry-lifetime-fixture"}, nil
}

// Exercise a real registry request and Agent RPC while the owning connection
// closes, then resolve again inside the late-helper registration window.
// The HTTP request must end because the proxy closes its sockets, not because
// the remote registry, request deadline or test cleanup eventually closes them.
func TestRegistryLifetimeTrafficAndLateRegistration(t *testing.T) {
	t.Setenv("WENDY_REGISTRY_CHAOS", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	httpEntered, httpStopped := make(chan struct{}), make(chan struct{})
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/fixture/blobs/held" {
			close(httpEntered)
			<-r.Context().Done()
			close(httpStopped)
			return
		}
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		_, _ = io.WriteString(w, "registry-lifetime-fixture")
	}))
	defer registry.Close()
	defer registry.CloseClientConnections()
	agent := &registryLifetimeAgent{entered: make(chan struct{}), stopped: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(server, agent)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer func() { server.Stop(); listener.Close(); <-serverDone }()
	connect := func() *grpcclient.AgentConnection {
		t.Helper()
		cc, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		conn := grpcclient.NewFromConn(cc)
		conn.RegistryDialer = func(ctx context.Context, _ int) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", registry.Listener.Addr().String())
		}
		return conn
	}
	conn := connect()
	defer conn.Close()
	address, _, _, err := resolveRegistryForAgent(ctx, conn, 5000)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + net.JoinHostPort("127.0.0.1", port)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	get := func(url string) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK || string(body) != "registry-lifetime-fixture" {
			return errors.New("wrong registry response")
		}
		return nil
	}
	if err := get(endpoint + "/v2/"); err != nil {
		t.Fatal(err)
	}
	reused, _, _, err := resolveRegistryForAgent(ctx, conn, 5000)
	if err != nil || reused != address {
		t.Fatalf("live connection did not reuse its registry proxy: %q %v", reused, err)
	}
	rpcResult, httpResult := make(chan error, 1), make(chan error, 1)
	go func() { httpResult <- get(endpoint + "/v2/fixture/blobs/held") }()
	go func() {
		_, err := conn.AgentService.GetAgentVersion(metadata.NewOutgoingContext(ctx, metadata.Pairs("regression-hold", "1")), &agentpb.GetAgentVersionRequest{})
		rpcResult <- err
	}()
	for _, entered := range []chan struct{}{httpEntered, agent.entered} {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("live registry/RPC fixture did not reach its response barrier")
		}
	}
	// Registered after the proxy: reaching this barrier proves its cache entry
	// has been evicted while AgentConnection.Close is still in progress.
	closing, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	conn.AddCloser(closeFunc(func() { close(closing); <-release }))
	closeResult := make(chan error, 1)
	go func() { closeResult <- conn.Close() }()
	select {
	case <-closing:
	case <-time.After(2 * time.Second):
		t.Fatal("connection close did not finish proxy cleanup")
	}
	for name, result := range map[string]<-chan error{"registry request": httpResult, "agent RPC": rpcResult} {
		select {
		case err := <-result:
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s did not terminate on connection close: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s survived owning connection close", name)
		}
	}
	for _, stopped := range []chan struct{}{httpStopped, agent.stopped} {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("remote registry/RPC handler survived connection close")
		}
	}
	late := make(chan error, 1)
	go func() {
		_, _, _, err := resolveRegistryForAgent(ctx, conn, 5000)
		late <- err
	}()
	select {
	case err := <-late:
		if err == nil {
			t.Fatal("late registration returned a proxy for the closed connection")
		}
	case <-time.After(time.Second):
		t.Fatal("late proxy registration deadlocked against cache eviction")
	}
	dockerRegistryProxyCacheMu.Lock()
	_, retained := dockerRegistryProxyAddrs[conn]
	dockerRegistryProxyCacheMu.Unlock()
	if retained {
		t.Fatal("closed connection retained a registry cache entry")
	}
	if socket, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), time.Second); err == nil {
		socket.Close()
		t.Fatal("closed connection retained its registry listener")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection close did not finish after releasing its final closer")
	}
	fresh := connect()
	defer fresh.Close()
	address, _, _, err = resolveRegistryForAgent(ctx, fresh, 5000)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ = net.SplitHostPort(address)
	if err := get("http://" + net.JoinHostPort("127.0.0.1", port) + "/v2/"); err != nil {
		t.Fatalf("fresh registry connection did not recover: %v", err)
	}
	version, err := fresh.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil || version.GetVersion() != "registry-lifetime-fixture" {
		t.Fatalf("fresh agent connection did not recover: %v, %v", version, err)
	}
}
