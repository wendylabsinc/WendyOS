package services

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

func TestMeshAppDialerNeverUsesLegacyOrCloudFallback(t *testing.T) {
	d, probes := testDialer()
	d.SetLocalMeshDialer(func(context.Context, int32, uint16) (net.Conn, error) {
		return nil, meshsession.ErrDenied
	})
	app := NewMeshAppDialer(d)
	if _, _, err := app.DialDevice(context.Background(), 215, 8080); !errors.Is(err, meshsession.ErrDenied) {
		t.Fatalf("denial = %v", err)
	}
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("app dial used legacy transport: %+v", probes)
	}
	d.SetLocalMeshDialer(func(context.Context, int32, uint16) (net.Conn, error) {
		conn, _ := net.Pipe()
		return conn, nil
	})
	conn, mode, err := app.DialDevice(context.Background(), 215, 8080)
	if err != nil || mode != "local-mesh" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	_ = conn.Close()
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("app dial used legacy transport: %+v", probes)
	}
}

func TestMeshAppDialerRetriesTransientKernelRouteFailure(t *testing.T) {
	d, probes := testDialer()
	attempts := 0
	d.SetLocalMeshDialer(func(context.Context, int32, uint16) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			return nil, &net.OpError{Op: "write", Net: "udp", Err: syscall.EHOSTUNREACH}
		}
		conn, other := net.Pipe()
		_ = other.Close()
		return conn, nil
	})
	conn, mode, err := NewMeshAppDialer(d).DialDevice(context.Background(), 215, 8080)
	if err != nil || mode != "local-mesh" || attempts != 2 {
		t.Fatalf("mode=%q err=%v attempts=%d", mode, err, attempts)
	}
	_ = conn.Close()
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("app dial used legacy transport: %+v", probes)
	}
}
