package cloudmcp

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type tunnelBackend struct{ deny atomic.Bool }

func (*tunnelBackend) Organization(context.Context, string) (Organization, error) {
	return Organization{Issuer: "https://auth.example/realms/test"}, nil
}
func (b *tunnelBackend) Authorize(_ context.Context, token, org, device, method string) (Access, error) {
	if token != "user-token" {
		return Access{}, status.Error(codes.Unauthenticated, "bad token")
	}
	return Access{OrganizationID: org, TenantID: testTenant, UserID: "user", ServiceSubject: "machine", Enabled: true, UserAllowed: !b.deny.Load(), ServiceAllowed: true, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (*tunnelBackend) Devices(context.Context, Access) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

type tunnelConnector struct {
	listener *bufconn.Listener
	calls    atomic.Int32
}

func (c *tunnelConnector) Connect(ctx context.Context, a Access, device string) (*grpcclient.AgentConnection, error) {
	c.calls.Add(1)
	conn, err := grpc.NewClient("passthrough:///test-device", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return c.listener.DialContext(ctx) }))
	if err != nil {
		return nil, err
	}
	return grpcclient.NewFromConn(conn), nil
}

type testAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpb.UnimplementedWendyShellServiceServer
	leaked      atomic.Bool
	correlation atomic.Value
	traceparent atomic.Value
}

func (s *testAgent) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.correlation.Store(strings.Join(md.Get("x-correlation-id"), ","))
	s.traceparent.Store(strings.Join(md.Get("traceparent"), ","))
	if len(md.Get("authorization")) != 0 || len(md.Get("x-wendy-client-cert")) != 0 {
		s.leaked.Store(true)
	}
	return &agentpb.GetAgentVersionResponse{Version: "hosted-test"}, nil
}
func (s *testAgent) HostShell(stream agentpb.WendyShellService_HostShellServer) error {
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: message.GetStdinData()}}); err != nil {
			return err
		}
	}
}

func TestTunnelProxiesAndRechecksAuthorization(t *testing.T) {
	upstream := grpc.NewServer()
	agent := &testAgent{}
	agentpb.RegisterWendyAgentServiceServer(upstream, agent)
	agentpb.RegisterWendyShellServiceServer(upstream, agent)
	listener := bufconn.Listen(1 << 20)
	go upstream.Serve(listener)
	t.Cleanup(upstream.Stop)
	backend := &tunnelBackend{}
	connector := &tunnelConnector{listener: listener}
	var server *Server
	httpServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { server.ServeHTTP(w, r) }))
	t.Cleanup(httpServer.Close)
	var err error
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(context.Background())
	server, err = New(httpServer.URL, backend, connector, WithTelemetry(nil, provider))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws, handshake, err := websocket.Dial(ctx, httpServer.URL+"/orgs/"+testOrg+"/devices/"+testDevice+"/tunnel", &websocket.DialOptions{HTTPClient: httpServer.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer user-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	socket := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	var dialled atomic.Bool
	conn, err := grpc.NewClient("passthrough:///hosted", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		if dialled.Swap(true) {
			return nil, io.EOF
		}
		return socket, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agentpb.NewWendyAgentServiceClient(conn)
	callCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "should-not-forward", "x-wendy-client-cert", "spoof"))
	version, err := client.GetAgentVersion(callCtx, &agentpb.GetAgentVersionRequest{})
	if err != nil || version.GetVersion() != "hosted-test" || agent.leaked.Load() {
		t.Fatalf("version=%v err=%v leaked=%v", version, err, agent.leaked.Load())
	}
	if agent.correlation.Load() != handshake.Header.Get("X-Request-ID") || !strings.HasPrefix(agent.traceparent.Load().(string), "00-") {
		t.Fatal("lost correlation across CLI tunnel")
	}
	shell, err := agentpb.NewWendyShellServiceClient(conn).HostShell(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first chunk", "second chunk"} {
		if err := shell.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_StdinData{StdinData: []byte(text)}}); err != nil {
			t.Fatal(err)
		}
		output, err := shell.Recv()
		if err != nil || string(output.GetStdoutData()) != text {
			t.Fatal(output, err)
		}
	}
	if err := shell.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := shell.Recv(); err != io.EOF {
		t.Fatalf("half-close lost: %v", err)
	}
	before := connector.calls.Load()
	err = conn.Invoke(ctx, "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo", &agentpb.GetAgentVersionRequest{}, &agentpb.GetAgentVersionResponse{})
	if status.Code(err) != codes.PermissionDenied || connector.calls.Load() != before {
		t.Fatalf("reflection reached device: %v", err)
	}
	backend.deny.Store(true)
	_, err = client.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if status.Code(err) != codes.PermissionDenied || connector.calls.Load() != before {
		t.Fatalf("revoked RPC reached device: %v", err)
	}
	conn.Close()
}
func TestMCPDeviceRPCUsesOperatorConnection(t *testing.T) {
	upstream := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(upstream, &testAgent{})
	listener := bufconn.Listen(1 << 20)
	go upstream.Serve(listener)
	defer upstream.Stop()
	connector := &tunnelConnector{listener: listener}
	server, err := New(ProductionURL, &tunnelBackend{}, connector)
	if err != nil {
		t.Fatal(err)
	}
	response := request(server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_rpc","arguments":{"device_id":"`+testDevice+`","method":"`+agentpb.WendyAgentService_GetAgentVersion_FullMethodName+`","request":{}}}}`, "Bearer user-token")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "hosted-test") || connector.calls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
}

func (*tunnelBackend) Record(context.Context, Access, GatewayEvent) error { return nil }
