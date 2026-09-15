package clouddefaults

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestBrokerTunnelConnReportsBrokerReasonOnRead(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewBrokerTunnelConn(local)
	defer conn.Close()
	reason := status.Error(codes.PermissionDenied, "user is not a current member of this organization")
	conn.Fail(reason)
	remote.Close()

	_, err := conn.Read(make([]byte, 1))
	var closed *TunnelClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("Read returned %v, want *TunnelClosedError", err)
	}
	if !errors.Is(err, reason) {
		t.Fatalf("Read error %v does not wrap the broker reason", err)
	}
	if status.Code(closed.Reason) != codes.PermissionDenied {
		t.Fatalf("Reason code = %v, want PermissionDenied", status.Code(closed.Reason))
	}
	// The verdict must not be discoverable as a gRPC status through errors.As:
	// gRPC's picker would then end RPCs with the broker's code (and hand the raw
	// connection error text to the presenter) instead of treating a failed
	// handshake as the transport failure it is.
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		t.Fatal("broker verdict leaks as a gRPC status through the connection error")
	}
	if !strings.Contains(err.Error(), "cloud tunnel closed by broker") || !strings.Contains(err.Error(), "not a current member") {
		t.Fatalf("Read error text %q does not explain the close", err.Error())
	}
}

func TestBrokerTunnelConnReportsBrokerReasonOnWrite(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewBrokerTunnelConn(local)
	defer conn.Close()
	reason := status.Error(codes.Unavailable, "asset not online")
	conn.Fail(reason)
	remote.Close()

	_, err := conn.Write([]byte("hello"))
	if !errors.Is(err, reason) {
		t.Fatalf("Write returned %v, want an error wrapping the broker reason", err)
	}
}

func TestBrokerTunnelConnKeepsEOFWithoutReason(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewBrokerTunnelConn(local)
	defer conn.Close()
	remote.Close()

	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read returned %v, want io.EOF when the broker closed cleanly", err)
	}
	var closed *TunnelClosedError
	if errors.As(err, &closed) {
		t.Fatal("clean close must not be reported as a broker failure")
	}
}

func TestBrokerTunnelConnIgnoresBenignFailures(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewBrokerTunnelConn(local)
	defer conn.Close()
	conn.Fail(nil)
	conn.Fail(io.EOF)
	conn.Fail(context.Canceled)
	conn.Fail(status.Error(codes.Canceled, "context canceled"))
	remote.Close()

	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read returned %v, want io.EOF: EOF/cancellation are not broker verdicts", err)
	}
}

func TestBrokerTunnelConnFirstReasonWins(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewBrokerTunnelConn(local)
	defer conn.Close()
	first := status.Error(codes.PermissionDenied, "first")
	conn.Fail(first)
	conn.Fail(status.Error(codes.Internal, "second"))
	remote.Close()

	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, first) {
		t.Fatalf("Read returned %v, want the first recorded reason", err)
	}
}

func TestBrokerTunnelConnPassesDataThrough(t *testing.T) {
	local, remote := net.Pipe()
	conn := NewBrokerTunnelConn(local)
	defer conn.Close()
	defer remote.Close()
	go func() { _, _ = remote.Write([]byte("ok")) }()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("ReadFull = %q, %v; want \"ok\", nil", buf, err)
	}
}

// The reason must reach the gRPC caller: a tunnel the broker refused shows up
// as the refusal, not as a bare EOF during the TLS handshake.
func TestTunnelDialerSurfacesBrokerReason(t *testing.T) {
	_, tlsConfig := tunnelHealthServer(t)
	reason := status.Error(codes.PermissionDenied, "user is not a current member of this organization")
	dialOpt := TunnelDialer(func(ctx context.Context) (net.Conn, error) {
		local, remote := net.Pipe()
		conn := NewBrokerTunnelConn(local)
		conn.Fail(reason)
		remote.Close()
		return conn, nil
	})
	conn, err := grpc.NewClient("passthrough:///cloud-tunnel", dialOpt,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("RPC over a refused tunnel succeeded")
	}
	t.Logf("RPC error as gRPC reports it: %v", err)
	if !strings.Contains(err.Error(), "not a current member of this organization") {
		t.Fatalf("RPC error %q hides the broker's refusal", err.Error())
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("RPC code = %v, want Unavailable: a refused tunnel is a transport failure to the RPC", status.Code(err))
	}
}

func TestExplainTunnelClose(t *testing.T) {
	cases := []struct {
		name, msg, want string
		ok              bool
	}{
		{"status verdict", `rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: cloud tunnel closed by broker: rpc error: code = PermissionDenied desc = user is not a current member of this organization"`,
			"user is not a current member of this organization (PermissionDenied)", true},
		{"reset verdict", `connection error: desc = "transport: authentication handshake failed: cloud tunnel closed by broker: rpc error: code = Internal desc = stream terminated by RST_STREAM with error code: INTERNAL_ERROR"`,
			"stream terminated by RST_STREAM with error code: INTERNAL_ERROR (Internal)", true},
		{"plain reason", "transport: authentication handshake failed: cloud tunnel closed by broker: context deadline exceeded",
			"context deadline exceeded", true},
		{"no marker", `connection error: desc = "transport: authentication handshake failed: EOF"`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExplainTunnelClose(tc.msg)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ExplainTunnelClose = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
