package meshoperatorlink

import (
	"errors"
	"io"
	"time"
)

func (l *Link) currentExpiry() time.Time {
	l.expiryMu.Lock()
	defer l.expiryMu.Unlock()
	return l.expires
}
func (l *Link) Live() bool {
	l.expiryMu.Lock()
	defer l.expiryMu.Unlock()
	return !l.closed && time.Now().Before(l.expires)
}

func (l *Link) expire() {
	l.expiryMu.Lock()
	if l.closed {
		l.expiryMu.Unlock()
		return
	}
	remaining := time.Until(l.expires)
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
	return writeControl(l.conn, m)
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
