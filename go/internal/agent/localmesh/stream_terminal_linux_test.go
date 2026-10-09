//go:build linux

package localmesh

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type terminalWriteConn struct {
	net.Conn
	failure  error
	entered  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (c *terminalWriteConn) Write([]byte) (int, error) {
	close(c.entered)
	<-c.release
	close(c.returned)
	return 0, c.failure
}

func terminalWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("stream worker did not finish")
	}
}

func terminalFixture(t *testing.T) (*streamNodeLink, *terminalWriteConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	c := &terminalWriteConn{Conn: a, failure: errors.New("injected send stalled"), entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	l := newStreamNodeLink(c)
	t.Cleanup(func() { _ = l.Close(); _ = b.Close() })
	return l, c, b
}

func TestStreamWriterCauseSurvivesInducedReadClose(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(map[bool]string{false: "datagram", true: "control"}[control], func(t *testing.T) {
			l, c, _ := terminalFixture(t)
			ack := make(chan error, 1)
			if control {
				go func() { ack <- l.WriteControl(ControlMessage{Kind: "hello", Hello: &LinkHello{Version: 1}}) }()
			} else {
				if err := l.SendDatagram([]byte{PacketBabel, 1}); err != nil {
					t.Fatal(err)
				}
			}
			terminalWait(t, c.entered)
			close(c.release)
			terminalWait(t, l.done)
			terminalWait(t, c.returned)
			if _, err := l.ReadControl(); !errors.Is(err, c.failure) || !strings.Contains(err.Error(), "mesh stream write:") {
				t.Fatalf("read lost initiating writer cause: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := l.ReceiveDatagram(ctx); !errors.Is(err, c.failure) {
				t.Fatalf("datagram lost writer cause: %v", err)
			}
			if control {
				select {
				case err := <-ack:
					if !errors.Is(err, c.failure) {
						t.Fatalf("control acknowledgement lost cause: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("control acknowledgement blocked")
				}
			}
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = l.Close()
					if !errors.Is(l.terminalError(), c.failure) {
						t.Error("concurrent Close replaced initiating failure")
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestStreamExplicitClosePrecedesInterruptedWriter(t *testing.T) {
	l, c, _ := terminalFixture(t)
	if err := l.SendDatagram([]byte{PacketBabel, 1}); err != nil {
		t.Fatal(err)
	}
	terminalWait(t, c.entered)
	_ = l.Close()
	terminalWait(t, l.done)
	close(c.release)
	terminalWait(t, c.returned)
	if err := l.terminalError(); !errors.Is(err, net.ErrClosed) || errors.Is(err, c.failure) {
		t.Fatalf("explicit Close mislabeled as writer failure: %v", err)
	}
}

func TestStreamReadFailurePrecedesWriterFailure(t *testing.T) {
	l, c, peer := terminalFixture(t)
	if err := l.SendDatagram([]byte{PacketBabel, 1}); err != nil {
		t.Fatal(err)
	}
	terminalWait(t, c.entered)
	_ = peer.Close()
	terminalWait(t, l.done)
	close(c.release)
	terminalWait(t, c.returned)
	if err := l.terminalError(); !errors.Is(err, io.EOF) || errors.Is(err, c.failure) {
		t.Fatalf("first read failure overwritten: %v", err)
	}
}
