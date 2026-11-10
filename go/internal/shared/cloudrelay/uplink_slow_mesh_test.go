package cloudrelay

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestUplinkFallbackAllowsSlowMeshHandshake(t *testing.T) {
	d := newUplinkDialer()
	// Accelerate only the initial system-route probe; keep the production
	// fallback budget and exercise the actual racing/cancellation path.
	d.timeout = 20 * time.Millisecond
	d.candidates = func(context.Context) ([]uplink, error) { return []uplink{{name: "mesh"}}, nil }
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn, err := d.connect(context.Background(), func(ctx context.Context, path uplink) (net.Conn, error) {
		if path.name == "system route" {
			return nil, errors.New("primary unavailable")
		}
		until, ok := ctx.Deadline()
		if !ok || time.Until(until) < 55*time.Second || time.Until(until) > 61*time.Second {
			t.Errorf("fallback handshake budget: %v, deadline present=%v", time.Until(until), ok)
		}
		select {
		case <-time.After(80 * time.Millisecond):
			return left, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil || conn != left {
		t.Fatalf("slow fallback: %v, %v", conn, err)
	}
}
