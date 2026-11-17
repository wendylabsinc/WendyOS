package directstream

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestRetainedConnSuspensionAndSharedBudget(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	a, err := b.NewConn(512, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.NewConn(512, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer other.Close()
	payload := bytes.Repeat([]byte{0x5a}, 768)
	done := make(chan error, 1)
	go func() {
		n, err := a.Write(payload)
		if n != len(payload) && err == nil {
			err = io.ErrShortWrite
		}
		done <- err
	}()
	deadline := time.After(time.Second)
	for {
		changed := b.Changed()
		var tail uint64
		a.WithFlow(func(s *Sender, r *Receiver) (bool, error) { tail = s.Buffer.End(); return false, nil })
		if tail == 512 {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("writer never filled budget")
		}
	}
	other.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	if n, err := other.Write([]byte{1}); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("shared budget: %d %v", n, err)
	}
	var wire [512]byte
	// Carrier offers bytes then disappears before ACK. The application socket
	// and its retained payload survive; the pending Write remains backpressured.
	if err := a.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		s.RemoteLimit = 1024
		n, err := s.Offer(wire[:], 0)
		if n != 512 {
			t.Fatalf("offer %d", n)
		}
		return false, err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("write completed without ACK: %v", err)
	default:
	}
	// Replacement reconciles 256 accepted bytes and replays the unaccepted suffix.
	if err := a.WithFlow(func(s *Sender, r *Receiver) (bool, error) { return true, s.State(256, 1024) }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK did not wake writer")
	}
	if err := a.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		n, err := s.Offer(wire[:], 256)
		if n != 512 || !bytes.Equal(wire[:n], payload[256:]) {
			t.Fatal("replay lost bytes")
		}
		return true, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := a.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		if !s.finished || s.final != 768 {
			t.Fatal("wrong FIN")
		}
		return true, s.State(768, 1024)
	}); err != nil {
		t.Fatal(err)
	}
	other.SetWriteDeadline(time.Time{})
	if n, err := other.Write([]byte{1}); n != 1 || err != nil {
		t.Fatalf("reclaimed shared budget: %d %v", n, err)
	}
}

func TestRetainedConnReceiveFINAndDeadlines(t *testing.T) {
	b, _ := NewConnBudget(256, 256)
	c, _ := b.NewConn(256, 256, nil, nil)
	defer c.Close()
	got := make(chan error, 1)
	go func() {
		p := make([]byte, 3)
		_, err := io.ReadFull(c, p)
		if err == nil && !bytes.Equal(p, []byte("abc")) {
			err = errors.New("wrong bytes")
		}
		got <- err
	}()
	c.WithFlow(func(s *Sender, r *Receiver) (bool, error) { return true, r.Finish(3) })
	select {
	case err := <-got:
		t.Fatalf("premature EOF: %v", err)
	default:
	}
	c.WithFlow(func(s *Sender, r *Receiver) (bool, error) { return true, r.Accept([]byte("abc")) })
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read stuck")
	}
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("EOF: %d %v", n, err)
	}
	d, _ := NewConnBudget(256, 256)
	pending, _ := d.NewConn(256, 256, nil, nil)
	defer pending.Close()
	go func() { _, err := pending.Read(make([]byte, 1)); got <- err }()
	pending.SetReadDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-got:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not wake read")
	}
	pending.SetReadDeadline(time.Time{})
	go func() { _, err := pending.Read(make([]byte, 1)); got <- err }()
	pending.Abort(net.ErrClosed)
	select {
	case err := <-got:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not wake read")
	}
	if d.rx.Available() != 256 || d.tx.Available() != 256 {
		t.Fatal("abort leaked slabs")
	}
}
