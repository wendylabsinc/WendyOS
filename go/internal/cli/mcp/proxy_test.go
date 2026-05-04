package mcp

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// fakeStreamMCPServer is a minimal gRPC server that echoes MCPChunk bytes back.
type fakeStreamMCPServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
}

func (f *fakeStreamMCPServer) StreamMCP(stream grpc.BidiStreamingServer[agentpb.MCPChunk, agentpb.MCPChunk]) error {
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&agentpb.MCPChunk{Data: chunk.Data}); err != nil {
			return err
		}
	}
}

func newFakeAgentConn(t *testing.T) (*grpcclient.AgentConnection, func()) {
	t.Helper()
	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(srv, &fakeStreamMCPServer{})
	go srv.Serve(lis)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("creating bufconn client: %v", err)
	}

	ac := &grpcclient.AgentConnection{
		Conn:             conn,
		ContainerService: agentpb.NewWendyContainerServiceClient(conn),
	}
	return ac, func() { conn.Close(); srv.Stop(); lis.Close() }
}

func TestStartMCPProxy_EchoesBytesThrough(t *testing.T) {
	conn, cleanup := newFakeAgentConn(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, closeProxy, err := startMCPProxy(ctx, conn, "my-app")
	if err != nil {
		t.Fatalf("startMCPProxy: %v", err)
	}
	defer closeProxy()

	// Connect a raw TCP client to the proxy address.
	tcpConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialing proxy: %v", err)
	}
	defer tcpConn.Close()

	payload := []byte("hello from test")
	if _, err := tcpConn.Write(payload); err != nil {
		t.Fatalf("writing payload: %v", err)
	}

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(tcpConn, buf); err != nil {
		t.Fatalf("reading echo: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("expected %q, got %q", payload, buf)
	}
}

func TestStartMCPProxy_AppNameInMetadata(t *testing.T) {
	metadataCh := make(chan string, 1)

	lis := bufconn.Listen(1024 * 1024)
	captureSrv := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(captureSrv, &metadataCaptureServer{onAppName: func(n string) {
		select {
		case metadataCh <- n:
		default:
		}
	}})
	go captureSrv.Serve(lis)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc client: %v", err)
	}
	defer conn.Close()
	defer captureSrv.Stop()
	defer lis.Close()

	ac := &grpcclient.AgentConnection{
		Conn:             conn,
		ContainerService: agentpb.NewWendyContainerServiceClient(conn),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addr, closeProxy, err := startMCPProxy(ctx, ac, "my-special-app")
	if err != nil {
		t.Fatalf("startMCPProxy: %v", err)
	}
	defer closeProxy()

	tcpConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialing proxy: %v", err)
	}
	tcpConn.Write([]byte("x"))
	tcpConn.Close()

	select {
	case name := <-metadataCh:
		if name != "my-special-app" {
			t.Fatalf("expected app-name 'my-special-app', got %q", name)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for app-name metadata")
	}
}

type metadataCaptureServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
	onAppName func(string)
}

func (m *metadataCaptureServer) StreamMCP(stream grpc.BidiStreamingServer[agentpb.MCPChunk, agentpb.MCPChunk]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	if vals := md.Get("app-name"); len(vals) > 0 {
		m.onAppName(vals[0])
	}
	return nil
}
