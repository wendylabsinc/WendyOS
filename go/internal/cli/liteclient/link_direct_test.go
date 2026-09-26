package liteclient

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// escapePrefixLen is the length of the echo mode command that prefixes every
// sentinel; sentinelWriteLen adds the 16 spaces of padding and the 32 hex
// characters of the sentinel itself.
const (
	escapePrefixLen  = 5
	sentinelWriteLen = escapePrefixLen + 16 + 32
)

// noisyHandshakePort models a freshly rebooted board over USB Serial JTAG.
//
// backlog is console output the device queued before echo mode took effect;
// draining it one byte at a time keeps Read continuously ready, so the
// handshake never sees the idle read the pre-65df7e0e implementation waited
// for. backlogDelay paces that drain, letting the handshake's resend timer fire
// while echoes are still stuck behind the backlog.
//
// swallow models the device refusing the echo mode command because its WendyCom
// agent is not running yet: that many sentinels are consumed in console mode,
// unechoed, before echo mode goes live.
type noisyHandshakePort struct {
	backlog      int           // console bytes still to emit before any echo
	backlogDelay time.Duration // per-byte pacing while the backlog drains
	swallow      int           // sentinels consumed before echo mode is live

	sentinels    [][]byte      // every sentinel written, in order
	escapeWrites int           // sentinel writes that re-armed echo mode
	readTimeout  time.Duration // last timeout the handshake asked for
	rx           []byte        // echoes, queued behind the backlog
	modeSwitched bool
	rxAtSwitch   int // bytes still unread when DLE m was written
}

func (p *noisyHandshakePort) Write(data []byte) (int, error) {
	if bytes.Equal(data, []byte{escapeChar, 'm'}) {
		p.modeSwitched = true
		p.rxAtSwitch = p.backlog + len(p.rx)
		return len(data), nil
	}
	if len(data) != sentinelWriteLen {
		return 0, fmt.Errorf("unexpected handshake write of %d bytes", len(data))
	}
	// Every sentinel has to re-arm echo mode: the device refuses the command
	// until its WendyCom agent runs, so sending it once is not enough.
	if !bytes.Equal(data[:escapePrefixLen], []byte{escapeChar, escapeChar, escapeChar, escapeChar, 'e'}) {
		return 0, errors.New("sentinel written without the echo mode command")
	}
	p.escapeWrites++
	p.sentinels = append(p.sentinels, append([]byte(nil), data[escapePrefixLen+16:]...))
	if p.swallow > 0 {
		p.swallow--
		return len(data), nil
	}
	// Echo mode echoes every received byte, padding included, but escape
	// commands are consumed by the mode parser and never echoed.
	p.rx = append(p.rx, data[escapePrefixLen:]...)
	return len(data), nil
}

func (p *noisyHandshakePort) Read(dst []byte) (int, error) {
	if len(p.sentinels) == 0 {
		return 0, errors.New("handshake read before sending a sentinel")
	}
	if p.backlog > 0 {
		p.backlog--
		time.Sleep(p.backlogDelay)
		dst[0] = 'x'
		return 1, nil
	}
	if len(p.rx) == 0 {
		// Block for the timeout the caller asked for, then report it, the way
		// a real port does. Returning instantly would busy-spin the handshake.
		time.Sleep(p.readTimeout)
		return 0, nil
	}
	dst[0] = p.rx[0]
	p.rx = p.rx[1:]
	return 1, nil
}

func (p *noisyHandshakePort) SetReadTimeout(d time.Duration) error {
	p.readTimeout = d
	return nil
}

// shrinkHandshakeTimings speeds up tests that need the resend scheduler to
// actually fire. The handshake timings are package vars for exactly this, which
// is also why no test in this file may run in parallel.
func shrinkHandshakeTimings(t *testing.T) {
	t.Helper()
	prevStep, prevMax, prevBudget := sentinelIntervalStep, sentinelIntervalMax, handshakeBudget
	sentinelIntervalStep = 10 * time.Millisecond
	sentinelIntervalMax = 50 * time.Millisecond
	handshakeBudget = 300 * time.Millisecond
	t.Cleanup(func() {
		sentinelIntervalStep, sentinelIntervalMax, handshakeBudget = prevStep, prevMax, prevBudget
	})
}

func TestNextSentinelInterval(t *testing.T) {
	for _, tc := range []struct{ current, want time.Duration }{
		{0, 100 * time.Millisecond},
		{100 * time.Millisecond, 200 * time.Millisecond},
		{200 * time.Millisecond, 300 * time.Millisecond},
		{300 * time.Millisecond, 400 * time.Millisecond},
		{400 * time.Millisecond, 500 * time.Millisecond},
		{500 * time.Millisecond, 500 * time.Millisecond},
	} {
		if got := nextSentinelInterval(tc.current); got != tc.want {
			t.Errorf("nextSentinelInterval(%s) = %s, want %s", tc.current, got, tc.want)
		}
	}
}

func TestSerialHandshakeSendsSentinelBeforeDrainingBootLogs(t *testing.T) {
	port := &noisyHandshakePort{backlog: 48}
	if err := serialHandshake(port); err != nil {
		t.Fatalf("serialHandshake() = %v", err)
	}
	if !port.modeSwitched {
		t.Fatal("handshake returned without switching to WendyCom mode")
	}
	if len(port.sentinels) != 1 {
		t.Errorf("sent %d sentinels, want 1", len(port.sentinels))
	}
}

// TestSerialHandshakeMatchesTheLastSentinelSent covers the guarantee the
// sentinel exists for: on return, the stream is drained and both sides are in
// sync. Matching any earlier sentinel leaves the later echoes queued, and the
// WendyCom framer reads their leading padding as a frame header.
func TestSerialHandshakeMatchesTheLastSentinelSent(t *testing.T) {
	port := &noisyHandshakePort{backlog: 25, backlogDelay: 10 * time.Millisecond}
	if err := serialHandshake(port); err != nil {
		t.Fatalf("serialHandshake() = %v", err)
	}
	if len(port.sentinels) < 2 {
		t.Fatalf("test did not exercise the race: only %d sentinel(s) sent", len(port.sentinels))
	}
	if !port.modeSwitched {
		t.Fatal("handshake returned without switching to WendyCom mode")
	}
	if port.rxAtSwitch != 0 {
		t.Errorf("%d unread byte(s) left in the stream at the mode switch, so an "+
			"earlier sentinel was matched", port.rxAtSwitch)
	}
	seen := make(map[string]bool, len(port.sentinels))
	for _, sentinel := range port.sentinels {
		if seen[string(sentinel)] {
			t.Fatalf("sentinel %q sent twice: a repeated sentinel cannot prove the stream drained", sentinel)
		}
		seen[string(sentinel)] = true
	}
}

// TestSerialHandshakeResendsAfterSwallowedSentinels covers the device refusing
// the echo mode command until its WendyCom agent is up, so the first sentinels
// are consumed in console mode and silently dropped.
func TestSerialHandshakeResendsAfterSwallowedSentinels(t *testing.T) {
	shrinkHandshakeTimings(t)
	port := &noisyHandshakePort{swallow: 2}
	if err := serialHandshake(port); err != nil {
		t.Fatalf("serialHandshake() = %v", err)
	}
	if len(port.sentinels) != 3 {
		t.Errorf("sent %d sentinels, want 3 (two swallowed, one echoed)", len(port.sentinels))
	}
	if port.escapeWrites != len(port.sentinels) {
		t.Errorf("%d of %d sentinels re-armed echo mode, want all: a device that "+
			"refused the first command would never recover",
			port.escapeWrites, len(port.sentinels))
	}
	if port.rxAtSwitch != 0 {
		t.Errorf("%d unread byte(s) left in the stream at the mode switch", port.rxAtSwitch)
	}
}

// TestSerialHandshakeBacksOffInsteadOfFlooding covers a device that never
// reaches its WendyCom agent: every echo mode command is refused, so no
// sentinel comes back and the handshake runs its whole budget. The resend
// interval has to widen over that budget rather than pushing a sentinel every
// step at a board that is still booting.
func TestSerialHandshakeBacksOffInsteadOfFlooding(t *testing.T) {
	shrinkHandshakeTimings(t)
	port := &noisyHandshakePort{swallow: 1000}
	if err := serialHandshake(port); err == nil {
		t.Fatal("serialHandshake() = nil, want a timeout error")
	}
	if port.modeSwitched {
		t.Error("handshake switched to WendyCom mode without ever matching a sentinel")
	}
	flooded := int(handshakeBudget / sentinelIntervalStep)
	if len(port.sentinels) >= flooded {
		t.Errorf("sent %d sentinels in %s, want well under %d: the resend interval is not backing off",
			len(port.sentinels), handshakeBudget, flooded)
	}
	if len(port.sentinels) < 4 {
		t.Errorf("sent only %d sentinels in %s: the resend scheduler stalled",
			len(port.sentinels), handshakeBudget)
	}
	if port.escapeWrites != len(port.sentinels) {
		t.Errorf("%d of %d sentinels re-armed echo mode, want all",
			port.escapeWrites, len(port.sentinels))
	}
}

// recordingConn is a minimal io.ReadWriteCloser that records every write, for
// exercising directLink's keep-alive without a real serial.Port: keep-alive
// writes go through the same plain io.Writer path as send, so no serial.Port
// methods are needed.
type recordingConn struct {
	mu     sync.Mutex
	writes [][]byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (c *recordingConn) Read([]byte) (int, error) {
	return 0, errors.New("recordingConn: read not supported")
}

func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.writes...)
}

func (c *recordingConn) countKeepAlives() int {
	n := 0
	for _, w := range c.snapshot() {
		if bytes.Equal(w, []byte{escapeChar, keepAliveCmd}) {
			n++
		}
	}
	return n
}

func TestKeepAliveFiresWhenIdle(t *testing.T) {
	prev := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = prev })

	conn := &recordingConn{}
	link := newSerialLinkConn(conn)
	if err := link.startKeepAlive(); err != nil {
		t.Fatalf("startKeepAlive() = %v", err)
	}
	t.Cleanup(func() {
		if err := link.close(); err != nil {
			t.Errorf("close() = %v", err)
		}
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && conn.countKeepAlives() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := conn.countKeepAlives(); n < 2 {
		t.Fatalf("got %d keep-alive writes in %s of idle time, want at least 2", n, time.Second)
	}
}

func TestKeepAlivePostponedBySend(t *testing.T) {
	prev := keepAliveInterval
	keepAliveInterval = 60 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = prev })

	conn := &recordingConn{}
	link := newSerialLinkConn(conn)
	if err := link.startKeepAlive(); err != nil {
		t.Fatalf("startKeepAlive() = %v", err)
	}
	t.Cleanup(func() {
		if err := link.close(); err != nil {
			t.Errorf("close() = %v", err)
		}
	})

	time.Sleep(40 * time.Millisecond)
	before := conn.countKeepAlives()
	if err := link.send(&wendypb.WendyComMessage{}); err != nil {
		t.Fatalf("send() = %v", err)
	}

	// A fresh interval starts from this send, not from startKeepAlive: no
	// additional keep-alive should appear before it elapses.
	time.Sleep(40 * time.Millisecond) // 40ms since the send
	if n := conn.countKeepAlives(); n != before {
		t.Fatalf("got %d keep-alive write(s) 40ms after send (had %d before), want no new ones: keep-alive was not postponed", n, before)
	}

	time.Sleep(40 * time.Millisecond) // 80ms since the send: past the interval
	if n := conn.countKeepAlives(); n <= before {
		t.Fatal("keep-alive never fired after the postponed interval elapsed")
	}
}

func TestKeepAliveStopsOnClose(t *testing.T) {
	prev := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = prev })

	conn := &recordingConn{}
	link := newSerialLinkConn(conn)
	if err := link.startKeepAlive(); err != nil {
		t.Fatalf("startKeepAlive() = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- link.close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close() did not return: keep-alive goroutine leaked")
	}

	before := len(conn.snapshot())
	time.Sleep(3 * keepAliveInterval)
	if after := len(conn.snapshot()); after != before {
		t.Errorf("write count grew from %d to %d after close: keep-alive kept running", before, after)
	}
}

var exitCmd = []byte{escapeChar, 'o'}

func TestCloseSendsExitAndRejectsWrites(t *testing.T) {
	conn := &recordingConn{}
	link := newSerialLinkConn(conn)
	if err := link.startKeepAlive(); err != nil {
		t.Fatalf("startKeepAlive() = %v", err)
	}
	if err := link.close(); err != nil {
		t.Fatalf("close() = %v", err)
	}

	writes := conn.snapshot()
	if len(writes) == 0 || !bytes.Equal(writes[len(writes)-1], exitCmd) {
		t.Fatalf("writes = %q, want the last one to be DLE 'o'", writes)
	}
	if err := link.send(&wendypb.WendyComMessage{}); !errors.Is(err, errLinkClosed) {
		t.Fatalf("send() after close = %v, want %v", err, errLinkClosed)
	}
	if n := len(conn.snapshot()); n != len(writes) {
		t.Fatalf("send() after close wrote to the port (%d writes, had %d)", n, len(writes))
	}
}

func TestStartKeepAliveAfterCloseIsRejected(t *testing.T) {
	conn := &recordingConn{}
	link := newSerialLinkConn(conn)
	if err := link.close(); err != nil {
		t.Fatalf("close() = %v", err)
	}
	if err := link.startKeepAlive(); !errors.Is(err, errLinkClosed) {
		t.Fatalf("startKeepAlive() after close = %v, want %v", err, errLinkClosed)
	}
	writes := conn.snapshot()
	if len(writes) != 1 || !bytes.Equal(writes[0], exitCmd) {
		t.Fatalf("writes = %q, want only DLE 'o'", writes)
	}
}

func TestCloseWithoutKeepAliveDoesNotWait(t *testing.T) {
	link := newSerialLinkConn(&recordingConn{})
	done := make(chan error, 1)
	go func() { done <- link.close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close() waited for a keep-alive loop that never started")
	}
}

func TestCloseWaitsForInFlightWrite(t *testing.T) {
	prev := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = prev })

	conn := &recordingConn{}
	link := newSerialLinkConn(conn)
	if err := link.startKeepAlive(); err != nil {
		t.Fatalf("startKeepAlive() = %v", err)
	}

	// Stand in for a send in flight; the keep-alive queues behind it on
	// writeMu once its interval elapses.
	link.writeMu.Lock()
	time.Sleep(3 * keepAliveInterval)

	done := make(chan error, 1)
	go func() { done <- link.close() }()
	select {
	case err := <-done:
		link.writeMu.Unlock()
		t.Fatalf("close() = %v returned while a write held writeMu", err)
	case <-time.After(50 * time.Millisecond):
	}

	link.writeMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close() did not return after the in-flight write finished")
	}

	writes := conn.snapshot()
	if len(writes) == 0 || !bytes.Equal(writes[len(writes)-1], exitCmd) {
		t.Fatalf("writes = %q, want DLE 'o' last with no keep-alive after it", writes)
	}
}

func TestCloseNonSerialRejectsWrites(t *testing.T) {
	conn := &recordingConn{}
	link := newDirectLink(conn)
	if err := link.close(); err != nil {
		t.Fatalf("close() = %v", err)
	}
	if err := link.send(&wendypb.WendyComMessage{}); !errors.Is(err, errLinkClosed) {
		t.Fatalf("send() after close = %v, want %v", err, errLinkClosed)
	}
	if n := len(conn.snapshot()); n != 0 {
		t.Fatalf("got %d writes on a closed non-serial link, want none", n)
	}
}

// stuckConn models a serial device that stopped draining: frame writes block
// until ResetOutputBuffer purges them, the way PURGE_TXABORT aborts a pending
// overlapped write on Windows. The 2-byte escape commands go through.
type stuckConn struct {
	recordingConn
	purge     chan struct{}
	purgeOnce sync.Once
	resets    atomic.Int32
	blocked   chan struct{} // closed when the first frame write blocks
	blockOnce sync.Once
}

func newStuckConn() *stuckConn {
	return &stuckConn{purge: make(chan struct{}), blocked: make(chan struct{})}
}

func (c *stuckConn) Write(p []byte) (int, error) {
	if len(p) > 2 {
		c.blockOnce.Do(func() { close(c.blocked) })
		<-c.purge
		return 0, errors.New("stuckConn: write aborted")
	}
	return c.recordingConn.Write(p)
}

func (c *stuckConn) ResetOutputBuffer() error {
	c.resets.Add(1)
	c.purgeOnce.Do(func() { close(c.purge) })
	return nil
}

func enableCloseWatchdog(t *testing.T) {
	t.Helper()
	prevEnabled, prevDelay := closeWatchdogEnabled, closeWatchdogDelay
	closeWatchdogEnabled, closeWatchdogDelay = true, 50*time.Millisecond
	t.Cleanup(func() { closeWatchdogEnabled, closeWatchdogDelay = prevEnabled, prevDelay })
}

func TestCloseWatchdogAbortsStuckWrite(t *testing.T) {
	enableCloseWatchdog(t)

	conn := newStuckConn()
	link := newSerialLinkConn(conn)
	sendErr := make(chan error, 1)
	go func() { sendErr <- link.send(&wendypb.WendyComMessage{}) }()
	<-conn.blocked

	done := make(chan error, 1)
	go func() { done <- link.close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close() did not return: the watchdog did not free the stuck write")
	}

	if err := <-sendErr; err == nil {
		t.Fatal("stuck send() returned nil after being purged, want an error")
	}
	if n := conn.resets.Load(); n < 1 {
		t.Fatalf("ResetOutputBuffer called %d times, want at least 1", n)
	}
	writes := conn.snapshot()
	if len(writes) == 0 || !bytes.Equal(writes[len(writes)-1], exitCmd) {
		t.Fatalf("writes = %q, want DLE 'o' last", writes)
	}
}

func TestCloseWatchdogIdleOnHealthyClose(t *testing.T) {
	enableCloseWatchdog(t)

	conn := newStuckConn()
	link := newSerialLinkConn(conn)
	if err := link.close(); err != nil {
		t.Fatalf("close() = %v", err)
	}
	time.Sleep(3 * closeWatchdogDelay)
	if n := conn.resets.Load(); n != 0 {
		t.Fatalf("ResetOutputBuffer called %d times on a healthy close, want 0", n)
	}
}

func TestCloseWatchdogRepeatsUntilStopped(t *testing.T) {
	enableCloseWatchdog(t)

	conn := newStuckConn()
	link := &directLink{conn: conn}
	stop := link.startCloseWatchdog()
	time.Sleep(3*closeWatchdogDelay + closeWatchdogDelay/2)
	stop()
	n := conn.resets.Load()
	if n < 2 {
		t.Fatalf("ResetOutputBuffer called %d times in 3.5 periods, want it repeated", n)
	}
	time.Sleep(3 * closeWatchdogDelay)
	if after := conn.resets.Load(); after != n {
		t.Fatalf("ResetOutputBuffer called %d more times after stop", after-n)
	}
}
