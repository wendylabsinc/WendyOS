package commands

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type foxgloveExecTestServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
	exec func(agentpb.WendyContainerService_ExecContainerServer) error
}

func (s *foxgloveExecTestServer) ExecContainer(stream agentpb.WendyContainerService_ExecContainerServer) error {
	return s.exec(stream)
}

func newFoxgloveExecTestClient(t *testing.T, exec func(agentpb.WendyContainerService_ExecContainerServer) error) agentpb.WendyContainerServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(server, &foxgloveExecTestServer{exec: exec})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient("passthrough:///foxglove-test",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentpb.NewWendyContainerServiceClient(conn)
}

func receiveFoxgloveExecStart(stream agentpb.WendyContainerService_ExecContainerServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	start := req.GetStart()
	if start == nil {
		return fmt.Errorf("first request is not an exec start: %v", req)
	}
	if start.GetAppName() != foxgloveAppID {
		return fmt.Errorf("exec container = %q", start.GetAppName())
	}
	if start.GetTty() || start.GetTermSize() != nil {
		return fmt.Errorf("binary forwarding requested a terminal: %v", start)
	}
	if want := []string{"socat", "STDIO", "TCP:127.0.0.1:8765"}; !reflect.DeepEqual(start.GetCommand(), want) {
		return fmt.Errorf("exec command = %q, want %q", start.GetCommand(), want)
	}
	return nil
}

func TestOpenFoxgloveConnectionBinaryRoundTrip(t *testing.T) {
	// Include every byte value and span many gRPC messages. A PTY would corrupt
	// control characters and newline sequences in a WebSocket connection.
	payload := make([]byte, 2<<20+37)
	for i := range payload {
		payload[i] = byte(i)
	}
	client := newFoxgloveExecTestClient(t, func(stream agentpb.WendyContainerService_ExecContainerServer) error {
		if err := receiveFoxgloveExecStart(stream); err != nil {
			return err
		}
		received := 0
		for received < len(payload) {
			req, err := stream.Recv()
			if err != nil {
				return err
			}
			frame, ok := req.GetRequestType().(*agentpb.ExecContainerRequest_StdinData)
			if !ok || len(frame.StdinData) == 0 {
				return fmt.Errorf("expected nonempty stdin data, got %v", req)
			}
			received += len(frame.StdinData)
			if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StderrData{StderrData: []byte("socat diagnostic\r\n")}}); err != nil {
				return err
			}
			if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: frame.StdinData}}); err != nil {
				return err
			}
		}
		return stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: 0}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := openFoxgloveConnection(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, bytes.NewReader(payload))
		written <- err
	}()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read forwarded data: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write forwarded data: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("binary round trip changed data or included stderr: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestOpenFoxgloveConnectionReportsExecFailure(t *testing.T) {
	client := newFoxgloveExecTestClient(t, func(stream agentpb.WendyContainerService_ExecContainerServer) error {
		if err := receiveFoxgloveExecStart(stream); err != nil {
			return err
		}
		if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StderrData{StderrData: []byte("connect: Connection refused\n")}}); err != nil {
			return err
		}
		return stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: 23}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := openFoxgloveConnection(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := io.ReadAll(conn)
	if len(got) != 0 {
		t.Fatalf("stderr leaked into forwarded bytes: %q", got)
	}
	if err == nil || !strings.Contains(err.Error(), "23") || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("read error = %v, want exit status and diagnostic", err)
	}
}

func TestServeFoxgloveForwardCancellationClosesClientsAndExecStreams(t *testing.T) {
	const connections = 2
	streamsClosed := make(chan struct{}, connections)
	client := newFoxgloveExecTestClient(t, func(stream agentpb.WendyContainerService_ExecContainerServer) error {
		defer func() { streamsClosed <- struct{}{} }()
		if err := receiveFoxgloveExecStart(stream); err != nil {
			return err
		}
		if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: []byte("ready")}}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener := bufconn.Listen(1 << 20)
	defer listener.Close()
	served := make(chan error, 1)
	go func() { served <- serveFoxgloveForward(ctx, listener, client) }()
	conns := make([]net.Conn, 0, connections)
	for i := 0; i < connections; i++ {
		conn, err := listener.DialContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conns = append(conns, conn)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		got := make([]byte, len("ready"))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != "ready" {
			t.Fatalf("connection %d did not become ready: data = %q, error = %v", i, got, err)
		}
	}
	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("forward server did not stop after cancellation")
	}
	for i, conn := range conns {
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Errorf("connection %d remained open after server stopped", i)
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Errorf("connection %d timed out instead of closing: %v", i, err)
		}
	}
	for i := 0; i < connections; i++ {
		select {
		case <-streamsClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("an exec stream remained active after cancellation")
		}
	}
	dialCtx, stopDial := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stopDial()
	if conn, err := listener.DialContext(dialCtx); err == nil {
		conn.Close()
		t.Error("forward listener remained open after cancellation")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Error("forward listener remained open without accepting connections")
	}
}

func TestWaitForFoxgloveBridgeAcceptsCurrentAndLegacySubprotocols(t *testing.T) {
	for _, selectedProtocol := range []string{"foxglove.sdk.v1", "foxglove.websocket.v1"} {
		t.Run(selectedProtocol, func(t *testing.T) {
			var attempts atomic.Int32
			probeClosed := make(chan struct{}, 1)
			client := newFoxgloveExecTestClient(t, func(stream agentpb.WendyContainerService_ExecContainerServer) error {
				if err := receiveFoxgloveExecStart(stream); err != nil {
					return err
				}
				if attempts.Add(1) == 1 {
					return stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: 1}})
				}
				var header []byte
				for !bytes.Contains(header, []byte("\r\n\r\n")) {
					req, err := stream.Recv()
					if err != nil {
						return err
					}
					header = append(header, req.GetStdinData()...)
					if len(header) > 16<<10 {
						return fmt.Errorf("oversized WebSocket handshake")
					}
				}
				req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
				if err != nil {
					return err
				}
				protocols := strings.Split(req.Header.Get("Sec-WebSocket-Protocol"), ",")
				for i := range protocols {
					protocols[i] = strings.TrimSpace(protocols[i])
				}
				if req.Method != http.MethodGet || req.Header.Get("Upgrade") != "websocket" || !reflect.DeepEqual(protocols, []string{"foxglove.sdk.v1", "foxglove.websocket.v1"}) {
					return fmt.Errorf("invalid Foxglove WebSocket probe: %s %v", req.Method, req.Header)
				}
				key := req.Header.Get("Sec-WebSocket-Key")
				if key == "" {
					return fmt.Errorf("WebSocket probe omitted handshake key")
				}
				digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				response := fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]), selectedProtocol)
				if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: []byte(response)}}); err != nil {
					return err
				}
				<-stream.Context().Done()
				probeClosed <- struct{}{}
				return stream.Context().Err()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := waitForFoxgloveBridge(ctx, client); err != nil {
				t.Fatalf("bridge never became ready: %v", err)
			}
			if got := attempts.Load(); got != 2 {
				t.Errorf("readiness attempts = %d, want 2", got)
			}
			select {
			case <-probeClosed:
			case <-ctx.Done():
				t.Fatal("readiness probe left its exec stream open")
			}
		})
	}
}
