//go:build darwin && cgo

package central

import (
	"errors"
	"net"
	"testing"
	"time"
)

// The net.Conn wrapper must reach the Darwin native setter, including the
// ordinary closed-handle result, rather than silently ignore its deadline.
func TestDarwinL2CAPStreamForwardsWriteDeadline(t *testing.T) {
	stream := NewL2CAPStream(&Connection{})
	if err := stream.SetWriteDeadline(time.Now().Add(time.Second)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write deadline on closed native connection: %v", err)
	}
	if err := stream.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("combined deadline must forward clearing the write deadline: %v", err)
	}
}
