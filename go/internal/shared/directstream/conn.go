package directstream

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// ConnBudget serializes application I/O with the session actor and protects the
// shared slabs. A session owns one budget; no stream creates a private window.
// Session code must fence carrier workers before taking recovery snapshots.
type ConnBudget struct {
	mu      sync.Mutex
	tx, rx  *Pool
	changed chan struct{}
	senders *RetainedConn
}

func NewConnBudget(txBytes, rxBytes int) (*ConnBudget, error) {
	tx, err := NewPool(txBytes)
	if err != nil {
		return nil, err
	}
	rx, err := NewPool(rxBytes)
	if err != nil {
		return nil, err
	}
	return &ConnBudget{tx: tx, rx: rx, changed: make(chan struct{})}, nil
}
func (b *ConnBudget) wake() { close(b.changed); b.changed = make(chan struct{}) }

// Called under mu, including application Write. This list is bounded by live
// managed flows and never reads the actor-owned flow map concurrently.
func (b *ConnBudget) transmitAvailable(owner *RetainedConn) uint64 {
	free := uint64(b.tx.Available())
	for c := b.senders; c != nil; c = c.sendNext {
		if c == owner || c.terminal != nil || c.promoted != nil || c.sender.finished {
			continue
		}
		buf := c.sender.Buffer
		held := buf.Limit() - buf.Start()/ChunkSize*ChunkSize
		if held < c.sendMinimum {
			deficit := c.sendMinimum - held
			if deficit > free {
				return 0
			}
			free -= deficit
		}
	}
	return free
}
func (c *RetainedConn) removeSendShare() {
	for at := &c.budget.senders; *at != nil; at = &(*at).sendNext {
		if *at == c {
			*at = c.sendNext
			c.sendNext = nil
			c.sendMinimum = 0
			return
		}
	}
}

// RetainedConn is the application's socket, whose lifetime is independent of
// the carrier. The actor drives Offer/Receive/State; a carrier error must pause
// that actor, not call Close on this socket. Terminal session loss calls Abort.
// The session must authorize and deduplicate OPEN before creating this object.
type RetainedConn struct {
	budget                      *ConnBudget
	sendNext                    *RetainedConn
	sendMinimum                 uint64
	sender                      Sender
	receiver                    Receiver
	window                      uint64
	readDeadline, writeDeadline time.Time
	local, remote               net.Addr
	terminal                    error
	promoted                    PromotedStream
	promoting                   bool

	promotedReadEOF, promotedWriteClosed, promotedRetired bool
	closeWriteMu                                          sync.Mutex
}

var _ net.Conn = (*RetainedConn)(nil)

func (b *ConnBudget) NewConn(window, initial uint64, local, remote net.Addr) (*RetainedConn, error) {
	return b.newConn(window, initial, local, remote, 0, nil)
}

func (b *ConnBudget) newConn(window, initial uint64, local, remote net.Addr, minimumTX uint64, admit func() bool) (*RetainedConn, error) {
	if window == 0 || window%ChunkSize != 0 || initial == 0 || initial > window || initial%ChunkSize != 0 || minimumTX > window || minimumTX%ChunkSize != 0 {
		return nil, ErrBounds
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if minimumTX > b.transmitAvailable(nil) || (admit != nil && !admit()) {
		return nil, ErrCapacity
	}
	rx := b.rx.NewBuffer()
	if err := rx.Reserve(initial); err != nil {
		return nil, err
	}
	c := &RetainedConn{budget: b, window: window, local: local, remote: remote, sender: Sender{Buffer: b.tx.NewBuffer()}, receiver: Receiver{Buffer: rx}}
	if minimumTX != 0 {
		c.sendMinimum = minimumTX
		c.sendNext = b.senders
		b.senders = c
	}
	b.wake()
	return c, nil
}

// wait returns with the shared lock held. Deadlines are checked by the caller
// before touching payloads, including after a concurrent deadline change.
func (c *RetainedConn) wait(deadline time.Time) error {
	b := c.budget
	changed := b.changed
	b.mu.Unlock()
	var err error
	if deadline.IsZero() {
		<-changed
	} else {
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-changed:
		case <-timer.C:
			err = os.ErrDeadlineExceeded
		}
		timer.Stop()
	}
	b.mu.Lock()
	return err
}
func expired(deadline time.Time) bool { return !deadline.IsZero() && !time.Now().Before(deadline) }
func (c *RetainedConn) Read(p []byte) (int, error) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if c.terminal != nil {
			return 0, c.terminal
		}
		if expired(c.readDeadline) {
			return 0, os.ErrDeadlineExceeded
		}
		if len(p) == 0 {
			return 0, nil
		}
		if c.promotedReadEOF {
			return 0, io.EOF
		}
		if c.receiver.Buffer.End() > c.receiver.Buffer.Start() {
			n, err := c.receiver.Buffer.CopyTo(p, c.receiver.Buffer.Start())
			if err != nil {
				return 0, err
			}
			if err = c.receiver.Buffer.Release(c.receiver.Buffer.Start() + uint64(n)); err != nil {
				return 0, err
			}
			if c.promoted != nil && c.receiver.EOF() {
				c.promotedReadEOF = true
				c.receiver.Buffer.Close()
			}
			// Growing receive credit is explicit actor policy, not an automatic private
			// window reservation here. Freed chunks can serve any stream in the session.
			b.wake()
			return n, nil
		}
		if c.receiver.EOF() {
			if c.promoted != nil {
				c.promotedReadEOF = true
				c.receiver.Buffer.Close()
			}
			return 0, io.EOF
		}
		if c.promoted != nil {
			c.receiver.Buffer.Close()
			raw := c.promoted
			b.mu.Unlock()
			n, err := raw.Read(p)
			c.promotedIOError(err, true)
			b.mu.Lock()
			if err == io.EOF {
				c.promotedReadEOF = true
				b.wake()
			}
			return n, err
		}
		if err := c.wait(c.readDeadline); err != nil && expired(c.readDeadline) {
			return 0, err
		}
	}
}
func (c *RetainedConn) Write(p []byte) (int, error) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	written := 0
	for len(p) > 0 {
		if c.terminal != nil {
			return written, c.terminal
		}
		if c.sender.finished {
			return written, net.ErrClosed
		}
		if expired(c.writeDeadline) {
			return written, os.ErrDeadlineExceeded
		}
		if c.promoted != nil {
			if c.promoting {
				if err := c.wait(c.writeDeadline); err != nil && expired(c.writeDeadline) {
					return written, err
				}
				continue
			}
			raw := c.promoted
			b.mu.Unlock()
			n, err := raw.Write(p)
			c.promotedIOError(err, false)
			b.mu.Lock()
			return written + n, err
		}
		pending := c.sender.Buffer.End() - c.sender.Buffer.Start()
		// Include unused space in the final already-owned chunk. Never allocate a
		// staging copy of the entire application write while waiting for ACKs.
		free := uint64(b.tx.Available())
		if c.sendMinimum != 0 {
			free = b.transmitAvailable(c)
		}
		capacity := c.sender.Buffer.Limit() - c.sender.Buffer.End() + free
		n := min(uint64(len(p)), c.window-pending, capacity)
		if n > 0 {
			if err := c.sender.Accept(p[:n], c.window); err != nil {
				return written, err
			}
			written += int(n)
			p = p[n:]
			b.wake()
			continue
		}
		if err := c.wait(c.writeDeadline); err != nil && expired(c.writeDeadline) {
			return written, err
		}
	}
	return written, nil
}
func (c *RetainedConn) CloseWrite() error {
	c.closeWriteMu.Lock()
	defer c.closeWriteMu.Unlock()
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	if c.promoted != nil && !c.promoting {
		if c.promotedWriteClosed {
			return nil
		}
		c.sender.Finish()
		raw := c.promoted
		b.mu.Unlock()
		err := raw.CloseWrite()
		c.promotedIOError(err, false)
		b.mu.Lock()
		if err == nil {
			c.promotedWriteClosed = true
			b.wake()
		}
		return err
	}
	c.sender.Finish()
	b.wake()
	return nil
}
func (c *RetainedConn) Close() error { c.Abort(net.ErrClosed); return nil }

// Abort is terminal. It wakes blocked app calls and returns slab ownership.
// The actor must stop delivering frames before aborting or retiring this flow.
func (c *RetainedConn) Abort(err error) {
	if err == nil {
		err = net.ErrClosed
	}
	b := c.budget
	b.mu.Lock()
	c.removeSendShare()
	var raw PromotedStream
	if c.terminal == nil {
		c.terminal = err
		c.sender.Buffer.Close()
		c.receiver.Buffer.Close()
		if !c.promotedRetired {
			raw = c.promoted
		}
		b.wake()
	}
	b.mu.Unlock()
	if raw != nil {
		_ = raw.Close()
	}
}
func (c *RetainedConn) LocalAddr() net.Addr  { return c.local }
func (c *RetainedConn) RemoteAddr() net.Addr { return c.remote }
func (c *RetainedConn) SetDeadline(t time.Time) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	b.wake()
	if c.promoted != nil {
		if c.promoting {
			return c.promoted.SetReadDeadline(t)
		}
		return c.promoted.SetDeadline(t)
	}
	return nil
}
func (c *RetainedConn) SetReadDeadline(t time.Time) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	c.readDeadline = t
	b.wake()
	if c.promoted != nil {
		return c.promoted.SetReadDeadline(t)
	}
	return nil
}
func (c *RetainedConn) SetWriteDeadline(t time.Time) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	c.writeDeadline = t
	b.wake()
	if c.promoted != nil && !c.promoting {
		return c.promoted.SetWriteDeadline(t)
	}
	return nil
}

// WithFlow runs one actor transition atomically with application reads/writes
// and every other flow using the budget. The callback must not block, retain
// buffer pointers for asynchronous access, or call Conn methods. Session code
// remains responsible for generation fencing, ACK scheduling and wire output.
// Return changed=true only when state changed and another waiter may progress.
func (c *RetainedConn) WithFlow(fn func(*Sender, *Receiver) (bool, error)) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	if c.promoted != nil {
		return ErrPromoted
	}
	changed, err := fn(&c.sender, &c.receiver)
	if changed {
		b.wake()
	}
	return err
}

// Changed is a wake-up hint, not a progress counter. Snapshot it before testing
// actor readiness; every waiter must recheck its own state under WithFlow.
func (b *ConnBudget) Changed() <-chan struct{} { b.mu.Lock(); defer b.mu.Unlock(); return b.changed }

// terminalError lets the session actor distinguish a concurrent application
// close from a protocol error. A flow-local close must not kill sibling flows.
func (c *RetainedConn) terminalError() error {
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.terminal
}
