package meshoperatorlink

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A slow carrier writer may hold the TLS frame lock while a packet or control
// frame drains. Babel/datagram admission must remain bounded and return promptly.
func TestRouterDatagramsBoundedWhileCarrierWriteIsBusy(t *testing.T) {
	link := &RouterLink{Link: &Link{done: make(chan struct{})}, transmit: make(chan []byte, 32)}
	link.writeMu.Lock()
	defer link.writeMu.Unlock()
	done := make(chan error, 1)
	payload := bytes.Repeat([]byte{7}, 1100)
	go func() {
		for range 1000 {
			if err := link.SendDatagram(payload); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("datagram admission blocked on the busy carrier writer")
	}
	payload[0] = 9
	if len(link.transmit) != 32 {
		t.Fatal("unbounded or missing admitted packet reserve")
	}
	if (<-link.transmit)[0] != 7 {
		t.Fatal("admission retained caller-owned bytes")
	}
	close(link.done)
	if err := link.SendDatagram(payload); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed carrier accepted datagram", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := link.ReceiveDatagram(ctx); err == nil {
		t.Fatal("closed carrier read did not terminate")
	}
}
