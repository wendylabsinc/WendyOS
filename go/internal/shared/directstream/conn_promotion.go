package directstream

import (
	"errors"
	"io"
	"net"
	"time"
)

var ErrPromoted = errors.New("direct flow has committed to its upper transport")

// PromotedStream is a verified, bound upper QUIC or TLS stream. Its transport
// owns reliability after commit and must support independent send-side EOF.
type PromotedStream interface {
	net.Conn
	CloseWrite() error
}

// CommitPromotion is called by the owning actor ONLY after authenticated
// attachment commit, complete offset reconciliation, and fencing/joining every
// old-generation reader/writer. The sender's Start must already equal the peer's
// received offset. This function does not authorize a transport or reconcile it.
// Success transfers ownership of raw permanently; failure leaves it with the
// caller. A committed transport failure is terminal, never BLE recovery.
//
// Unread receive bytes remain before raw input. The missing send suffix drains
// before any new writes; each accepted byte releases replay ownership without
// a custom ACK. done closes when that prefix worker has exited, including on
// failure. The session must join it during retirement.
func (c *RetainedConn) CommitPromotion(raw PromotedStream) (<-chan struct{}, error) {
	if raw == nil {
		return nil, ErrBounds
	}
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return nil, c.terminal
	}
	if c.promoted != nil {
		return nil, ErrPromoted
	}
	if err := raw.SetReadDeadline(c.readDeadline); err != nil {
		return nil, err
	}
	if err := raw.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	c.removeSendShare()
	c.promoted = raw
	c.promotedReadEOF = c.receiver.EOF()
	if c.promotedReadEOF {
		c.receiver.Buffer.Close()
	}
	c.promoting = true
	done := make(chan struct{})
	b.wake()
	go c.drainPromotedPrefix(raw, done)
	return done, nil
}
func (c *RetainedConn) drainPromotedPrefix(raw PromotedStream, done chan struct{}) {
	defer close(done)
	b := c.budget
	var scratch [MaxDataPayload]byte
	for {
		b.mu.Lock()
		if c.terminal != nil {
			b.mu.Unlock()
			return
		}
		offset := c.sender.Buffer.Start()
		n, err := c.sender.Buffer.CopyTo(scratch[:], offset)
		if err != nil {
			b.mu.Unlock()
			c.Abort(err)
			return
		}
		if n == 0 {
			c.sender.Buffer.Close()
			finished := c.sender.finished
			if finished {
				b.mu.Unlock()
				err = raw.CloseWrite()
				b.mu.Lock()
			}
			if err == nil {
				err = raw.SetWriteDeadline(c.writeDeadline)
			}
			if err == nil {
				c.promoting = false
				c.promotedWriteClosed = finished
				b.wake()
			}
			b.mu.Unlock()
			if err != nil {
				c.Abort(err)
			}
			return
		}
		b.mu.Unlock()
		written, writeErr := raw.Write(scratch[:n])
		b.mu.Lock()
		if c.terminal != nil {
			b.mu.Unlock()
			return
		}
		if written < 0 || written > n {
			err = io.ErrShortWrite
		} else if written > 0 {
			err = c.sender.Buffer.Release(offset + uint64(written))
		}
		b.wake()
		b.mu.Unlock()
		if err != nil {
			c.Abort(err)
			return
		}
		if writeErr != nil {
			c.Abort(writeErr)
			return
		}
		if written == 0 {
			c.Abort(io.ErrNoProgress)
			return
		}
	}
}

// EOF is a receive half-close and a deadline is retryable. Other errors from
// an already committed transport end its logical stream; never retain it for
// attachment to a replacement carrier/upper connection.
func (c *RetainedConn) promotedIOError(err error, reading bool) {
	if err == nil || (reading && errors.Is(err, io.EOF)) {
		return
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return
	}
	c.Abort(err)
}

// Upper FIN/EOF discharges byte ownership without custom acknowledgements.
// Read EOF is recorded only once retained input has been delivered to the app;
// CloseWrite completion is separate from the app's intent to finish writing.
func (c *RetainedConn) promotedFinished() bool {
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.promoted != nil && !c.promoting && c.promotedReadEOF && c.promotedWriteClosed && c.terminal == nil
}

// retirePromoted closes an orderly completed upper transport while retaining
// EOF semantics for the app socket. An explicit app/session abort remains an
// error. The actor calls this only after bilateral flow close confirmation.
func (c *RetainedConn) retirePromoted() bool {
	b := c.budget
	b.mu.Lock()
	if c.promoted == nil || c.promoting || !c.promotedReadEOF || !c.promotedWriteClosed || c.terminal != nil {
		b.mu.Unlock()
		return false
	}
	raw := c.promoted
	already := c.promotedRetired
	c.promotedRetired = true
	b.wake()
	b.mu.Unlock()
	if !already {
		_ = raw.Close()
	}
	return true
}
