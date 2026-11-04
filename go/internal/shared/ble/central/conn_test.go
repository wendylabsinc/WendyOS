package central

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestL2CAPReadDeadlineMayChangeConcurrently(t *testing.T) {
	stream := NewL2CAPStream(&Connection{})
	expired := time.Now().Add(-time.Second)
	if err := stream.SetReadDeadline(expired); err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		<-started
		for i := 0; i < 1000; i++ {
			// On Darwin, the native write setter reports the unopened handle;
			// the read deadline still updates without touching any hardware.
			_ = stream.SetDeadline(expired.Add(-time.Duration(i)))
		}
	}()
	close(started)
	for i := 0; i < 1000; i++ {
		_, err := stream.Read(make([]byte, 1))
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("read with expired deadline: %v", err)
			break
		}
	}
	<-done
}

type blockingL2CAP struct {
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	ioMu    sync.Mutex
}

func (f *blockingL2CAP) L2CAPSend([]byte) error {
	f.ioMu.Lock()
	defer f.ioMu.Unlock()
	close(f.started)
	<-f.release
	return net.ErrClosed
}
func (f *blockingL2CAP) L2CAPRecv(int) ([]byte, error) { return nil, ErrRecvTimeout }
func (f *blockingL2CAP) Close() {
	close(f.release)
	f.ioMu.Lock()
	defer f.ioMu.Unlock()
	close(f.closed)
}

func TestStreamCloseInterruptsNativeWrite(t *testing.T) {
	f := &blockingL2CAP{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	c := &l2capNetConn{conn: f}
	written := make(chan error, 1)
	go func() { _, err := c.Write([]byte("TLS record")); written <- err }()
	<-f.started
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt the native write")
	}
	if err := <-written; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("interrupted write: %v", err)
	}
	if _, err := c.Write([]byte("late record")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
	c.Close()
}

func TestStreamReadDeadlineMayChangeDuringRead(t *testing.T) {
	f := &blockingL2CAP{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	c := &l2capNetConn{conn: f}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c.SetReadDeadline(time.Now().Add(time.Hour))
		}
		c.SetDeadline(time.Now().Add(-time.Second))
	}()
	_, err := c.Read(make([]byte, 1))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("read: %v", err)
	}
	wg.Wait()
	c.Close()
}
