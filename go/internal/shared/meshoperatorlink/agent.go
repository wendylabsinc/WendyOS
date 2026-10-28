package meshoperatorlink

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const agentWindow = 32 * 1024
const agentFramePayload = 1100 - 4 // Stream ID shares the existing1100-byte frame budget.

var ErrAgentUnavailable = errors.New("adjacent operator peer has no management stream")

// agentStream is created only by an admitted Link, never a caller-supplied
// identity/header. Its bounded credit ring lets packet and control reads keep
// running while management consumers are slow. One HTTP/2 connection carries
// concurrent management RPCs; it needs no second TLS handshake.
type agentStream struct {
	link                                     *Link
	cancel                                   context.CancelFunc
	id                                       uint32
	mu                                       sync.Mutex
	writeMu                                  sync.Mutex
	buffer                                   [agentWindow]byte
	head, size                               int
	receiveCredit, pendingCredit, sendCredit uint32
	ready                                    chan error
	done                                     chan struct{}
	readWake, writeWake                      chan struct{}
	closed                                   bool
	inFrame                                  bool
	writeTimer                               *time.Timer
	readDeadline, writeDeadline              time.Time
}

func newAgentStream(l *Link, id, sendCredit uint32) *agentStream {
	return &agentStream{link: l, id: id, receiveCredit: agentWindow, sendCredit: sendCredit,
		ready: make(chan error, 1), done: make(chan struct{}), readWake: make(chan struct{}, 1), writeWake: make(chan struct{}, 1)}
}
func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// OpenAgent opens management on this exact adjacent peer. The RouterLink
// reader continues draining carrier frames independently. Setup cancellation retires only the
// stream; attachment expiry/cancellation retires every stream.
func (l *Link) OpenAgent(ctx context.Context) (net.Conn, error) {
	if l.server {
		return nil, errors.New("only an operator opens management")
	}
	l.agentMu.Lock()
	if l.agentClosing || !l.Live() {
		l.agentMu.Unlock()
		return nil, io.ErrClosedPipe
	}
	if l.agent != nil {
		l.agentMu.Unlock()
		return nil, errors.New("operator management stream already open")
	}
	if l.agentID == ^uint32(0) {
		l.agentMu.Unlock()
		return nil, errors.New("operator management stream IDs exhausted")
	}
	l.agentID++
	c := newAgentStream(l, l.agentID, 0)
	l.agent = c
	l.agentMu.Unlock()
	if err := l.queueControl(control{Kind: "operator-agent-open", Stream: c.id, Credit: agentWindow}); err != nil {
		c.close(false)
		return nil, err
	}
	select {
	case err := <-c.ready:
		if err != nil {
			c.close(false)
			return nil, err
		}
		return c, nil
	case <-c.done:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case <-l.done:
		c.close(false)
		return nil, io.ErrClosedPipe
	}
}
func (l *Link) closeAgent() {
	l.agentMu.Lock()
	l.agentClosing = true
	c := l.agent
	l.agentMu.Unlock()
	if c != nil {
		c.close(false)
	}
}
func (l *Link) currentAgent(id uint32) *agentStream {
	l.agentMu.Lock()
	defer l.agentMu.Unlock()
	if l.agent != nil && l.agent.id == id {
		return l.agent
	}
	return nil
}
func (l *Link) agentControl(m control) (bool, error) {
	switch m.Kind {
	case "operator-agent-open":
		if !l.server || m.Stream == 0 || m.Credit == 0 || m.Credit > agentWindow {
			return true, errors.New("invalid operator agent open")
		}
		l.agentMu.Lock()
		if l.agentClosing {
			l.agentMu.Unlock()
			return true, io.ErrClosedPipe
		}
		if m.Stream <= l.agentID {
			l.agentMu.Unlock()
			return true, nil
		}
		l.agentID = m.Stream
		reason := ""
		if l.agentHandler == nil {
			reason = "unavailable"
		} else if l.agent != nil || l.agentServing {
			reason = "busy"
		}
		if reason != "" {
			l.agentMu.Unlock()
			return true, l.queueControl(control{Kind: "operator-agent-ready", Stream: m.Stream, Error: reason})
		}
		c := newAgentStream(l, m.Stream, m.Credit)
		handlerCtx, cancel := context.WithCancel(l.agentContext)
		c.cancel = cancel
		l.agent = c
		l.agentServing = true
		// Close marks agentClosing under this same mutex before Wait can run.
		l.workers.Add(1)
		l.agentMu.Unlock()
		if err := l.queueControl(control{Kind: "operator-agent-ready", Stream: c.id, Credit: agentWindow}); err != nil {
			c.close(false)
			l.agentMu.Lock()
			l.agentServing = false
			l.agentMu.Unlock()
			l.workers.Done()
			return true, err
		}
		go func() {
			defer l.workers.Done()
			defer func() { c.Close(); l.agentMu.Lock(); l.agentServing = false; l.agentMu.Unlock() }()
			_ = l.agentHandler(handlerCtx, c)
		}()
		return true, nil
	case "operator-agent-ready":
		if l.server {
			return true, errors.New("unexpected operator agent ready")
		}
		c := l.currentAgent(m.Stream)
		if c == nil {
			return true, nil
		}
		if m.Error != "" {
			select {
			case c.ready <- ErrAgentUnavailable:
			default:
			}
			return true, nil
		}
		if m.Credit == 0 || m.Credit > agentWindow {
			return true, errors.New("invalid operator agent window")
		}
		c.mu.Lock()
		c.sendCredit = m.Credit
		c.mu.Unlock()
		wake(c.writeWake)
		select {
		case c.ready <- nil:
		default:
		}
		return true, nil
	case "operator-agent-credit":
		c := l.currentAgent(m.Stream)
		if c == nil {
			return true, nil
		}
		c.mu.Lock()
		if m.Credit == 0 || m.Credit > agentWindow-c.sendCredit {
			c.mu.Unlock()
			return true, errors.New("invalid operator agent credit")
		}
		c.sendCredit += m.Credit
		c.mu.Unlock()
		wake(c.writeWake)
		return true, nil
	case "operator-agent-close":
		if c := l.currentAgent(m.Stream); c != nil {
			c.close(false)
		}
		return true, nil
	default:
		return false, nil
	}
}
func (l *Link) agentData(data []byte) error {
	if len(data) <= 4 || len(data) > agentFramePayload+4 {
		return errors.New("invalid operator agent data")
	}
	c := l.currentAgent(binary.BigEndian.Uint32(data[:4]))
	if c == nil {
		return nil
	} // Retired stream's crossing bytes.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	n := len(data) - 4
	if uint32(n) > c.receiveCredit || n > len(c.buffer)-c.size {
		return errors.New("operator agent receive credit exceeded")
	}
	c.receiveCredit -= uint32(n)
	tail := (c.head + c.size) % len(c.buffer)
	first := min(n, len(c.buffer)-tail)
	copy(c.buffer[tail:], data[4:4+first])
	copy(c.buffer[:], data[4+first:])
	c.size += n
	wake(c.readWake)
	return nil
}
func (l *Link) agentCredit() (control, bool) {
	l.agentMu.Lock()
	c := l.agent
	l.agentMu.Unlock()
	if c == nil {
		return control{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.pendingCredit == 0 {
		return control{}, false
	}
	n := c.pendingCredit
	c.pendingCredit = 0
	// Reserve before writing: a fast remote can use a credit before Write returns.
	c.receiveCredit += n
	return control{Kind: "operator-agent-credit", Stream: c.id, Credit: n}, true
}
func waitAgent(wake, done, linkDone <-chan struct{}, deadline time.Time) error {
	var timer *time.Timer
	var elapsed <-chan time.Time
	if !deadline.IsZero() {
		if !time.Now().Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		timer = time.NewTimer(time.Until(deadline))
		elapsed = timer.C
		defer timer.Stop()
	}
	select {
	case <-wake:
		return nil
	case <-done:
		return io.ErrClosedPipe
	case <-linkDone:
		return io.ErrClosedPipe
	case <-elapsed:
		return os.ErrDeadlineExceeded
	}
}
func (c *agentStream) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	for {
		c.mu.Lock()
		if !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline) {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if c.size > 0 {
			n := min(len(out), c.size)
			first := min(n, len(c.buffer)-c.head)
			copy(out, c.buffer[c.head:c.head+first])
			copy(out[first:n], c.buffer[:n-first])
			c.head = (c.head + n) % len(c.buffer)
			c.size -= n
			c.pendingCredit += uint32(n)
			if c.size > 0 {
				wake(c.readWake)
			}
			c.mu.Unlock()
			wake(c.link.agentWake)
			return n, nil
		}
		closed, deadline := c.closed, c.readDeadline
		c.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		if err := waitAgent(c.readWake, c.done, c.link.done, deadline); err != nil {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if closed {
				continue
			} // Drain bytes received before the remote close.
			return 0, err
		}
	}
}
func (c *agentStream) Write(data []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	total := 0
	for len(data) > 0 {
		c.mu.Lock()
		closed, deadline := c.closed, c.writeDeadline
		past := !deadline.IsZero() && !time.Now().Before(deadline)
		n := min(len(data), agentFramePayload, int(c.sendCredit))
		if !closed && !past && n > 0 {
			c.sendCredit -= uint32(n)
			c.inFrame = true
		}
		c.mu.Unlock()
		if closed {
			return total, io.ErrClosedPipe
		}
		if past {
			return total, os.ErrDeadlineExceeded
		}
		if n == 0 {
			if err := waitAgent(c.writeWake, c.done, c.link.done, deadline); err != nil {
				return total, err
			}
			continue
		}
		body := make([]byte, 4+n)
		binary.BigEndian.PutUint32(body, c.id)
		copy(body[4:], data[:n])
		c.link.writeMu.Lock()
		err := c.link.writeFrame(3, body)
		c.link.writeMu.Unlock()
		c.mu.Lock()
		c.inFrame = false
		expired := !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline)
		c.mu.Unlock()
		if err != nil && expired {
			c.link.Close()
			return total, os.ErrDeadlineExceeded
		}
		if err != nil {
			c.link.Close()
			return total, err
		}
		total += n
		data = data[n:]
	}
	return total, nil
}
func (c *agentStream) close(notify bool) error {
	c.mu.Lock()
	// Local Close retires the caller's buffers immediately. A remote close is
	// ordered after its DATA frames, which remain readable before EOF.
	if notify {
		c.size = 0
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	inFrame := c.inFrame
	if c.writeTimer != nil {
		c.writeTimer.Stop()
	}
	close(c.done)
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.link.agentMu.Lock()
	if c.link.agent == c {
		c.link.agent = nil
	}
	c.link.agentMu.Unlock()
	if inFrame {
		// A partially written shared frame cannot be abandoned and resumed by
		// another stream. Closing its transport is the only way to unblock the
		// writer while retaining framing, matching the write-deadline policy.
		// Link.Close marks itself closed before closeAgent, so that path must
		// not re-enter its sync.Once while retiring the stream.
		if c.link.Live() {
			c.link.Close()
		}
		return nil
	}
	if notify {
		if err := c.link.queueControl(control{Kind: "operator-agent-close", Stream: c.id}); err != nil {
			c.link.Close()
			return err
		}
	}
	return nil
}
func (c *agentStream) Close() error         { return c.close(true) }
func (c *agentStream) LocalAddr() net.Addr  { return c.link.conn.LocalAddr() }
func (c *agentStream) RemoteAddr() net.Addr { return c.link.conn.RemoteAddr() }
func (c *agentStream) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.armWriteDeadline()
	c.mu.Unlock()
	wake(c.readWake)
	wake(c.writeWake)
	return nil
}
func (c *agentStream) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	wake(c.readWake)
	return nil
}
func (c *agentStream) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.armWriteDeadline()
	c.mu.Unlock()
	wake(c.writeWake)
	return nil
}

// Called under mu. A deadline while waiting for this stream's credits only
// wakes its writer. If a shared physical frame write is itself stuck, closing
// that transport is the only way to retire it without leaving a late writer.
func (c *agentStream) armWriteDeadline() {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	if c.closed || c.writeDeadline.IsZero() {
		return
	}
	deadline := c.writeDeadline
	c.writeTimer = time.AfterFunc(max(time.Until(deadline), 0), func() {
		c.mu.Lock()
		if c.closed || !c.writeDeadline.Equal(deadline) {
			c.mu.Unlock()
			return
		}
		inFrame := c.inFrame
		c.mu.Unlock()
		wake(c.writeWake)
		if inFrame {
			c.link.Close()
		}
	})
}
