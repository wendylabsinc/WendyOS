package liteclient

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
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
