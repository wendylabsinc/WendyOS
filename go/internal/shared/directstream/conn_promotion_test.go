package directstream

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// Independent pipe directions give deterministic backpressure and real
// half-close semantics without relying on a kernel's socket-buffer size.
type promotionPipe struct{ in, out net.Conn }

func promotionPair() (*promotionPipe, *promotionPipe) {
	a, b := net.Pipe()
	c, d := net.Pipe()
	return &promotionPipe{a, d}, &promotionPipe{c, b}
}
func (p *promotionPipe) Read(b []byte) (int, error)  { return p.in.Read(b) }
func (p *promotionPipe) Write(b []byte) (int, error) { return p.out.Write(b) }
func (p *promotionPipe) Close() error                { p.in.Close(); return p.out.Close() }
func (p *promotionPipe) CloseWrite() error           { return p.out.Close() }
func (p *promotionPipe) LocalAddr() net.Addr         { return p.in.LocalAddr() }
func (p *promotionPipe) RemoteAddr() net.Addr        { return p.in.RemoteAddr() }
func (p *promotionPipe) SetDeadline(t time.Time) error {
	p.in.SetReadDeadline(t)
	return p.out.SetWriteDeadline(t)
}
func (p *promotionPipe) SetReadDeadline(t time.Time) error  { return p.in.SetReadDeadline(t) }
func (p *promotionPipe) SetWriteDeadline(t time.Time) error { return p.out.SetWriteDeadline(t) }
func promotionRead(t *testing.T, r io.Reader, want string) {
	t.Helper()
	b := make([]byte, len(want))
	if _, err := io.ReadFull(r, b); err != nil || string(b) != want {
		t.Fatalf("got %q want %q: %v", b, want, err)
	}
}
func promotionWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("promotion worker did not join")
	}
}
func promotionBudget(t *testing.T, b *ConnBudget) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tx.Available() != 512 || b.rx.Available() != 512 {
		t.Fatalf("retained slabs tx=%d rx=%d", b.tx.Available(), b.rx.Available())
	}
}
func TestPromotionPreservesBothPrefixesAndStopsCustomRetention(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	c, _ := b.NewConn(512, 256, nil, nil)
	defer c.Close()
	if _, err := c.Write([]byte("ACKEDsuffix")); err != nil {
		t.Fatal(err)
	}
	if err := c.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		if err := s.State(0, 512); err != nil {
			return false, err
		}
		var wire [512]byte
		if _, err := s.Offer(wire[:], 0); err != nil {
			return false, err
		}
		if err := s.State(5, 512); err != nil {
			return false, err
		}
		return true, r.Accept([]byte("prefix"))
	}); err != nil {
		t.Fatal(err)
	}
	raw, peer := promotionPair()
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(time.Second))
	done, err := c.CommitPromotion(raw)
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { _, err := c.Write([]byte("new")); written <- err }()
	incoming := make(chan error, 1)
	go func() { _, err := peer.Write([]byte("tail")); incoming <- err }()
	promotionRead(t, c, "prefix")
	promotionRead(t, c, "tail")
	if err := <-incoming; err != nil {
		t.Fatal(err)
	}
	promotionRead(t, peer, "suffixnew")
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	promotionWait(t, done)
	promotionBudget(t, b)
	if err := c.WithFlow(func(*Sender, *Receiver) (bool, error) { t.Fatal("old lane mutated promoted bytes"); return false, nil }); !errors.Is(err, ErrPromoted) {
		t.Fatal(err)
	}
	extra, other := promotionPair()
	defer extra.Close()
	defer other.Close()
	if _, err := c.CommitPromotion(extra); !errors.Is(err, ErrPromoted) {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	peer.CloseWrite()
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
}
func TestPromotionAcceptedPrefixIgnoresNewApplicationWriteDeadline(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	c, _ := b.NewConn(512, 256, nil, nil)
	defer c.Close()
	c.Write([]byte("old"))
	raw, peer := promotionPair()
	defer peer.Close()
	done, err := c.CommitPromotion(raw)
	if err != nil {
		t.Fatal(err)
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	if n, err := c.Write([]byte("new")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%d %v", n, err)
	}
	c.SetWriteDeadline(time.Time{})
	peer.SetReadDeadline(time.Now().Add(time.Second))
	promotionRead(t, peer, "old")
	promotionWait(t, done)
	sent := make(chan error, 1)
	go func() { _, err := c.Write([]byte("new")); sent <- err }()
	promotionRead(t, peer, "new")
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}
func TestPromotionFailureOrAbortIsTerminalAndJoinsPrefixWorker(t *testing.T) {
	for _, abort := range []bool{false, true} {
		b, _ := NewConnBudget(512, 512)
		c, _ := b.NewConn(512, 256, nil, nil)
		c.Write([]byte("retained"))
		raw, peer := promotionPair()
		done, err := c.CommitPromotion(raw)
		if err != nil {
			t.Fatal(err)
		}
		if abort {
			c.Close()
		} else {
			peer.Close()
		}
		promotionWait(t, done)
		if _, err := c.Write([]byte("must not resume")); err == nil {
			t.Fatal("committed failure recovered")
		}
		promotionBudget(t, b)
		peer.Close()
		c.Close()
	}
}
func TestPromotionPreservesAlreadyReceivedFIN(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	c, _ := b.NewConn(512, 256, nil, nil)
	defer c.Close()
	c.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		s.Finish()
		r.Accept([]byte("last"))
		return true, r.Finish(4)
	})
	raw, peer := promotionPair()
	defer peer.Close()
	done, err := c.CommitPromotion(raw)
	if err != nil {
		t.Fatal(err)
	}
	promotionRead(t, c, "last")
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	promotionWait(t, done)
	promotionBudget(t, b)
}

func TestPromotionTransportDeathAfterPrefixIsTerminal(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	c, _ := b.NewConn(512, 256, nil, nil)
	defer c.Close()
	raw, peer := promotionPair()
	done, err := c.CommitPromotion(raw)
	if err != nil {
		t.Fatal(err)
	}
	promotionWait(t, done)
	peer.Close()
	if _, err := c.Write([]byte("new")); err == nil {
		t.Fatal("dead upper transport accepted bytes")
	}
	if c.terminalError() == nil {
		t.Fatal("transport death remained recoverable")
	}
	promotionBudget(t, b)
}

func TestPromotionPreservesRepeatedEOFWithRetainedFIN(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	app, _ := b.NewConn(512, 256, nil, nil)
	defer app.Close()
	if err := app.WithFlow(func(_ *Sender, r *Receiver) (bool, error) {
		if err := r.Accept([]byte("end")); err != nil {
			return false, err
		}
		return true, r.Finish(3)
	}); err != nil {
		t.Fatal(err)
	}
	raw, peer := promotionPair()
	defer peer.Close()
	done, err := app.CommitPromotion(raw)
	if err != nil {
		t.Fatal(err)
	}
	promotionWait(t, done)
	app.SetReadDeadline(time.Now().Add(time.Second))
	promotionRead(t, app, "end")
	for i := 0; i < 3; i++ {
		var p [1]byte
		if n, err := app.Read(p[:]); n != 0 || err != io.EOF {
			t.Fatal("FIN was forgotten after buffer release", n, err)
		}
	}
}

type retryCloseWrite struct {
	*promotionPipe
	calls int
}

func (p *retryCloseWrite) CloseWrite() error {
	p.calls++
	if p.calls == 1 {
		return os.ErrDeadlineExceeded
	}
	return p.promotionPipe.CloseWrite()
}
func TestPromotionRetriesTimedOutCloseWrite(t *testing.T) {
	b, _ := NewConnBudget(512, 512)
	app, _ := b.NewConn(512, 256, nil, nil)
	defer app.Close()
	raw, peer := promotionPair()
	defer peer.Close()
	transport := &retryCloseWrite{promotionPipe: raw}
	done, err := app.CommitPromotion(transport)
	if err != nil {
		t.Fatal(err)
	}
	promotionWait(t, done)
	if err = app.CloseWrite(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	if app.promotedFinished() {
		t.Fatal("timed out FIN completed flow")
	}
	if err = app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err = app.CloseWrite(); err != nil || transport.calls != 2 {
		t.Fatal("close retry not idempotent", err, transport.calls)
	}
	var p [1]byte
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := peer.Read(p[:]); n != 0 || err != io.EOF {
		t.Fatal(n, err)
	}
}
