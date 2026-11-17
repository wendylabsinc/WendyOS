package directstream

import (
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// DatagramBudget is separate from reliable replay slabs: UDP saturation cannot
// consume the storage reserved for TCP or control. A peer owns one budget.
// Each association reserves one TX, one assembly and one delivered-message slot.
type DatagramBudget struct {
	mu          sync.Mutex
	limit, used int
	changed     chan struct{}
}

func NewDatagramBudget(bytes int) (*DatagramBudget, error) {
	if bytes < 0 {
		return nil, ErrBounds
	}
	return &DatagramBudget{limit: bytes, changed: make(chan struct{})}, nil
}
func (b *DatagramBudget) wake() { close(b.changed); b.changed = make(chan struct{}) }
func (b *DatagramBudget) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}
func (b *DatagramBudget) Used() int { b.mu.Lock(); defer b.mu.Unlock(); return b.used }
func (b *DatagramBudget) NewConn(flow uint32, maximum int, local, remote net.Addr) (*DatagramConn, error) {
	if flow == 0 || maximum < 1 || maximum > 65535 {
		return nil, ErrBounds
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	charge := 3 * maximum
	if charge > b.limit-b.used {
		return nil, ErrCapacity
	}
	tx, _ := NewDatagramSender(flow, make([]byte, maximum))
	rx, _ := NewDatagramReceiver(flow, make([]byte, maximum))
	c := &DatagramConn{done: make(chan struct{}), budget: b, tx: tx, rx: rx, ready: make([]byte, maximum), charge: charge, maximum: maximum, local: local, remote: remote}
	b.used += charge
	return c, nil
}

// DatagramConn preserves an admitted app association across carrier changes.
// The session actor alone calls Bind/Suspend/Prepare/Accepted/Receive. App I/O
// may run concurrently. These methods do not authenticate or admit a flow.
type DatagramConn struct {
	done                        chan struct{}
	budget                      *DatagramBudget
	tx                          *DatagramSender
	rx                          *DatagramReceiver
	ready                       []byte
	readySize, charge, maximum  int
	hasReady                    bool
	local, remote               net.Addr
	readDeadline, writeDeadline time.Time
	terminal                    error
	droppedTX, droppedRX        uint64
	upper                       net.Conn // message-preserving promoted association, never a raw byte stream
	promotionDone               chan struct{}
}

var _ net.Conn = (*DatagramConn)(nil)

// Done closes when the logical association retires, never on carrier loss.
func (c *DatagramConn) Maximum() int { return c.maximum }

func (c *DatagramConn) Done() <-chan struct{} { return c.done }

func (c *DatagramConn) LocalAddr() net.Addr  { return c.local }
func (c *DatagramConn) RemoteAddr() net.Addr { return c.remote }
func (c *DatagramConn) Bind(generation uint64, alias uint32) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	if c.upper != nil {
		return ErrPromoted
	}
	if generation == 0 || generation <= c.tx.generation || generation <= c.rx.generation || alias == 0 {
		return ErrBounds
	}
	if c.tx.active {
		c.droppedTX++
	}
	if c.rx.active {
		c.droppedRX++
	}
	_ = c.tx.Bind(generation, alias)
	_ = c.rx.Bind(generation, alias)
	b.wake()
	return nil
}
func (c *DatagramConn) Suspend() {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return
	}
	if c.upper != nil {
		return
	}
	if c.tx.active {
		c.droppedTX++
	}
	if c.rx.active {
		c.droppedRX++
	}
	c.tx.Suspend()
	c.rx.Suspend()
	b.wake()
}
func (c *DatagramConn) Write(p []byte) (int, error) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return 0, c.terminal
	}
	if expired(c.writeDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if len(p) > c.maximum {
		return 0, syscall.EMSGSIZE
	}
	if c.upper != nil {
		raw := c.upper
		b.mu.Unlock()
		n, err := raw.Write(p)
		b.mu.Lock()
		return n, err
	}
	if c.tx.alias == 0 || c.tx.active {
		c.droppedTX++
		return len(p), nil
	}
	if err := c.tx.Queue(p); err != nil {
		return 0, err
	}
	b.wake()
	return len(p), nil
}
func (c *DatagramConn) Read(p []byte) (int, error) {
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
		if c.hasReady {
			n := copy(p, c.ready[:c.readySize])
			c.hasReady = false
			c.readySize = 0
			if c.upper != nil {
				c.ready = nil
				b.used -= c.charge
				c.charge = 0
			}
			b.wake()
			return n, nil
		}
		if c.upper != nil {
			raw := c.upper
			b.mu.Unlock()
			n, err := raw.Read(p)
			b.mu.Lock()
			return n, err
		}
		changed, deadline := b.changed, c.readDeadline
		b.mu.Unlock()
		if deadline.IsZero() {
			<-changed
		} else {
			timer := time.NewTimer(time.Until(deadline))
			select {
			case <-changed:
			case <-timer.C:
			}
			timer.Stop()
		}
		b.mu.Lock()
	}
}

// Prepare encodes into caller-owned wire storage so a later app write cannot
// mutate a fragment already submitted to the physical carrier. The caller must
// fence/join old carrier workers before binding a new generation.
func (c *DatagramConn) Prepare(dst []byte) (int, uint64, uint64, uint16, error) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return 0, 0, 0, 0, c.terminal
	}
	if c.upper != nil {
		return 0, 0, 0, 0, ErrPromoted
	}
	frame, ok := c.tx.Prepare()
	if !ok {
		return 0, 0, 0, 0, nil
	}
	n, err := frame.Encode(dst)
	return n, c.tx.generation, frame.Value, frame.FragmentOffset, err
}
func (c *DatagramConn) Accepted(generation, id uint64, offset uint16) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	if c.upper != nil {
		return ErrPromoted
	}
	if err := c.tx.Accepted(generation, id, offset); err != nil {
		return err
	}
	b.wake()
	return nil
}
func (c *DatagramConn) Receive(generation uint64, f FlowFrame) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	if c.upper != nil {
		return ErrPromoted
	}
	data, ready, err := c.rx.Receive(generation, f)
	if err != nil {
		return err
	}
	if ready {
		if c.hasReady {
			c.droppedRX++
		} else {
			c.readySize = copy(c.ready, data)
			c.hasReady = true
			b.wake()
		}
	}
	return nil
}
func (c *DatagramConn) Abort(err error) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return
	}
	if err == nil {
		err = net.ErrClosed
	}
	c.terminal = err
	close(c.done)
	c.tx = nil
	c.rx = nil
	c.ready = nil
	c.hasReady = false
	b.used -= c.charge
	c.charge = 0
	b.wake()
	if c.upper != nil {
		raw := c.upper
		b.mu.Unlock()
		_ = raw.Close()
		b.mu.Lock()
	}
	if c.promotionDone != nil {
		close(c.promotionDone)
	}
}

func (c *DatagramConn) Close() error                       { c.Abort(net.ErrClosed); return nil }
func (c *DatagramConn) SetDeadline(t time.Time) error      { return c.deadline(t, true, true) }
func (c *DatagramConn) SetReadDeadline(t time.Time) error  { return c.deadline(t, true, false) }
func (c *DatagramConn) SetWriteDeadline(t time.Time) error { return c.deadline(t, false, true) }
func (c *DatagramConn) deadline(t time.Time, read, write bool) error {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	if c.upper != nil {
		var err error
		if read && write {
			err = c.upper.SetDeadline(t)
		} else if read {
			err = c.upper.SetReadDeadline(t)
		} else {
			err = c.upper.SetWriteDeadline(t)
		}
		if err != nil {
			return err
		}
	}
	if read {
		c.readDeadline = t
	}
	if write {
		c.writeDeadline = t
	}
	b.wake()
	return nil
}

// Drops reports local queue/outage discards, not all network loss. Fragment
// loss and peer-side drops require end-to-end message-ID measurements.
func (c *DatagramConn) Drops() (tx, rx uint64) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	return c.droppedTX, c.droppedRX
}

// CommitPromotion takes an authenticated, bound, message-preserving upper
// association after the old carrier's I/O is joined and Suspend is called.
// raw must implement datagram Read/Write (including empty messages), not an
// unframed TLS/QUIC byte stream. The caller owns framing and upper association
// identity. Failure leaves raw with the caller. There is no UDP replay prefix.
// The same application object survives; BLE rebind is permanently forbidden.
func (c *DatagramConn) CommitPromotion(raw net.Conn) (<-chan struct{}, error) {
	b := c.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.terminal != nil {
		return nil, c.terminal
	}
	if raw == nil || raw == c {
		return nil, ErrBounds
	}
	if c.upper != nil {
		return nil, ErrPromoted
	}
	if c.tx.alias != 0 || c.rx.alias != 0 || c.tx.active || c.rx.active {
		return nil, ErrBounds
	}
	if err := raw.SetReadDeadline(c.readDeadline); err != nil {
		return nil, err
	}
	if err := raw.SetWriteDeadline(c.writeDeadline); err != nil {
		return nil, err
	}
	c.upper = raw
	c.promotionDone = make(chan struct{})
	// Keep only an already-complete packet awaiting its application Read. The
	// upper transport now owns its buffers; direct TX/reassembly are released.
	c.tx.storage = nil
	c.rx.storage = nil
	retained := 0
	if c.hasReady {
		retained = c.maximum
	} else {
		c.ready = nil
	}
	b.used -= c.charge - retained
	c.charge = retained
	b.wake()
	return c.promotionDone, nil
}
