package meshoperatorlink

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"io"
	"net"
	"time"
)

func (l *Link) currentExpiry() time.Time {
	l.expiryMu.Lock()
	deadline := l.transportExpiryLocked()
	retire := !l.closed && l.invalid != nil
	l.expiryMu.Unlock()
	if retire {
		_ = l.Close()
	}
	return deadline
}

// An explicit certificate clock never becomes a network deadline. Convert its
// remaining lease to the actual monotonic clock, capped by the initial lease.
func (l *Link) transportExpiryLocked() time.Time {
	if l.certNow == nil {
		return l.expires
	}
	started := time.Now()
	if l.invalid == nil {
		expires := l.expires
		if l.verifyExpiry != nil {
			var err error
			expires, err = l.verifyExpiry()
			if err != nil {
				l.invalid = err
			}
		}
		if l.invalid == nil {
			l.maxExpiry = minTime(l.maxExpiry, started.Add(expires.Sub(l.certNow())))
			if !time.Now().Before(l.maxExpiry) {
				l.invalid = errors.New("operator credential expired")
			}
		}
	}
	if l.invalid != nil {
		l.maxExpiry = minTime(l.maxExpiry, started)
	}
	return l.maxExpiry
}

func (l *Link) nextExpiryCheckLocked() time.Duration {
	remaining := time.Until(l.transportExpiryLocked())
	if l.certNow != nil {
		// Direct time correction may shorten a lease while all IO is idle.
		return min(remaining, time.Second)
	}
	return remaining
}

func (l *Link) Live() bool {
	l.expiryMu.Lock()
	live := !l.closed && time.Now().Before(l.transportExpiryLocked()) && l.invalid == nil
	retire := l.certNow != nil && !l.closed && !live
	l.expiryMu.Unlock()
	if retire {
		_ = l.Close()
	}
	return live
}

func (l *Link) policyLive() bool { return l.certNow == nil || l.Live() }

func (l *Link) expire() {
	l.expiryMu.Lock()
	if l.closed {
		l.expiryMu.Unlock()
		return
	}
	remaining := l.nextExpiryCheckLocked()
	if remaining > 0 {
		l.timer.Reset(remaining)
		l.expiryMu.Unlock()
		return
	}
	l.expiryMu.Unlock()
	l.Close()
}
func (l *Link) writeControl(m control) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	data, err := netwire.Marshal(m)
	if err != nil {
		return err
	}
	return l.writeFrame(1, data)
}

// A reader never writes to its full-duplex transport. Its bounded reply goes
// to a separate writer, so ordinary simultaneous IP writes cannot make both
// TLS readers wait for each other while returning management credit.
func (l *Link) queueControl(m control) error {
	select {
	case <-l.done:
		return io.ErrClosedPipe
	default:
	}
	select {
	case l.outControl <- m:
		return nil
	case <-l.done:
		return io.ErrClosedPipe
	default:
		return errors.New("operator control reply capacity reached")
	}
}
func (l *Link) controlWriter() {
	defer l.workers.Done()
	for {
		select {
		case <-l.done:
			return
		case <-l.agentWake:
			if m, ok := l.agentCredit(); ok {
				if err := l.writeControl(m); err != nil {
					l.Close()
					return
				}
			}
		case m := <-l.outControl:
			if err := l.writeControl(m); err != nil {
				l.Close()
				return
			}
		}
	}
}

// writeFrame is called under writeMu. A long credential lease is not a carrier
// write-progress allowance: a full physical send buffer must retire within the
// same finite budget as ordinary framed mesh streams.
func (l *Link) writeFrame(kind byte, data []byte) error {
	if !l.policyLive() {
		return net.ErrClosed
	}
	budget := 8 * time.Second
	if kind != 4 {
		budget = min(budget+time.Duration((len(data)+1023)/1024)*2*time.Second, 45*time.Second)
	}
	expires := l.currentExpiry()
	deadline := minTime(expires, time.Now().Add(budget))
	if err := l.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := writeFrame(l.conn, kind, data); err != nil {
		return err
	}
	return l.conn.SetWriteDeadline(expires)
}
