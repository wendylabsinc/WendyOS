package liteclient

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// connectedOverPipe returns a client whose read loop runs over one end of a
// net.Pipe, as after a successful connect, and the device's end of the pipe.
func connectedOverPipe(t *testing.T) (*WendyLiteClient, net.Conn) {
	t.Helper()
	host, device := net.Pipe()
	c := NewWendyLiteClient()
	c.link = newDirectLink(host)
	c.startReadLoop()
	t.Cleanup(func() {
		device.Close()
		c.Close()
	})
	return c, device
}

func TestDoneStaysOpenWithoutAConnection(t *testing.T) {
	select {
	case <-NewWendyLiteClient().Done():
		t.Fatal("Done closed on a client that never connected")
	default:
	}
}

func TestDoneClosesWhenTheDeviceDropsOff(t *testing.T) {
	c, device := connectedOverPipe(t)
	select {
	case <-c.Done():
		t.Fatal("Done closed while the link is up")
	case <-time.After(50 * time.Millisecond):
	}

	device.Close()
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after the device dropped off")
	}
	// Done fires after failAll, so commands are already refused.
	if err := c.Ping(); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("Ping after Done: got %v, want a connection lost error", err)
	}
}

func TestDoneClosesOnClose(t *testing.T) {
	c, _ := connectedOverPipe(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done still open after Close returned")
	}
}

func TestWaitRebootReturnsAtOnceOffSerial(t *testing.T) {
	c, _ := connectedOverPipe(t)
	start := time.Now()
	c.WaitReboot(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("non-serial WaitReboot blocked for %v", elapsed)
	}
}

func TestWaitRebootWaitsForSerialPeer(t *testing.T) {
	for _, end := range []string{"link dropped", "cancelled"} {
		t.Run(end, func(t *testing.T) {
			c := NewWendyLiteClient()
			c.link = &directLink{isSerial: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan struct{})
			go func() {
				c.WaitReboot(ctx)
				close(returned)
			}()
			select {
			case <-returned:
				t.Fatal("closed the serial link before the peer handled REBOOT")
			case <-time.After(50 * time.Millisecond):
			}
			if end == "link dropped" {
				close(c.done)
			} else {
				cancel()
			}
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatalf("WaitReboot ignored %s", end)
			}
		})
	}
}
