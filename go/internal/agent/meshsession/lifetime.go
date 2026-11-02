package meshsession

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// A certificate clock is not a scheduling deadline. Once projected onto the
// actual monotonic clock, a session's resource bound can only become shorter.
type sessionLifetime struct {
	mu       sync.Mutex
	deadline time.Time
	check    func() (time.Time, error)
	done     <-chan struct{}
	failure  error
}

func (l *sessionLifetime) current() (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failure != nil {
		return l.deadline, l.failure
	}
	if l.done != nil {
		select {
		case <-l.done:
			l.failure = net.ErrClosed
			return l.deadline, l.failure
		default:
		}
	}
	if l.check != nil {
		next, err := l.check()
		if err != nil {
			l.failure = err
			return l.deadline, err
		}
		if next.Before(l.deadline) {
			l.deadline = next
		}
	}
	if !time.Now().Before(l.deadline) {
		l.failure = errors.New("net app credentials expired")
	}
	return l.deadline, l.failure
}

func (l *sessionLifetime) delay(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if l.check != nil {
		return min(time.Second, remaining)
	}
	return remaining
}

// The watcher has one owner and exits with that session, including failed
// setup. Its expiry callback must retire/cancel, never join this watcher.
func (l *sessionLifetime) watch(done <-chan struct{}, expire func()) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			deadline, err := l.current()
			if err != nil {
				expire()
				return
			}
			timer := time.NewTimer(max(l.delay(deadline), 0))
			select {
			case <-done:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return stopped
}

func newSessionLifetime(credentials *localmesh.Credentials, state tls.ConnectionState) (*sessionLifetime, error) {
	chain := make([][]byte, len(state.PeerCertificates))
	for i, certificate := range state.PeerCertificates {
		chain[i] = bytes.Clone(certificate.Raw)
	}
	deadline, peer, err := sessionDeadline(credentials, chain)
	if err != nil {
		return nil, err
	}
	lifetime := &sessionLifetime{deadline: deadline}
	if credentials.HasCertificateTimePolicy() {
		lifetime.check = func() (time.Time, error) {
			current, currentPeer, err := sessionDeadline(credentials, chain)
			if err == nil && currentPeer != peer {
				err = errors.New("net app session identity changed")
			}
			return current, err
		}
	}
	return lifetime, nil
}

// TLS authenticates at handshake, but app streams can outlive a certificate.
// Recheck explicit policy while idle, including after direct clock correction.
func limitSessionLifetime(credentials *localmesh.Credentials, conn *quic.Conn) (*sessionLifetime, error) {
	lifetime, err := newSessionLifetime(credentials, conn.ConnectionState().TLS)
	if err != nil {
		return nil, err
	}
	lifetime.done = conn.Context().Done()
	lifetime.watch(conn.Context().Done(), func() { _ = conn.CloseWithError(1, "net app credentials expired") })
	return lifetime, nil
}

func verifiedSessionExpiry(credentials *localmesh.Credentials, state tls.ConnectionState) (time.Time, error) {
	chain := make([][]byte, len(state.PeerCertificates))
	for i, certificate := range state.PeerCertificates {
		chain[i] = bytes.Clone(certificate.Raw)
	}
	deadline, _, err := sessionDeadline(credentials, chain)
	return deadline, err
}

func sessionDeadline(credentials *localmesh.Credentials, chain [][]byte) (time.Time, meshid.Device, error) {
	// Capture scheduling time before verification; crypto work consumes, rather
	// than extends, the selected credential's remaining lifetime.
	started := time.Now()
	if credentials == nil {
		return time.Time{}, meshid.Device{}, errors.New("missing net app credentials")
	}
	local, err := credentials.Verify(credentials.Certificate.Certificate, started)
	if err != nil {
		return time.Time{}, meshid.Device{}, err
	}
	peer, err := credentials.VerifyTLS(chain, started)
	if err != nil {
		return time.Time{}, meshid.Device{}, err
	}
	if local.Device.Principal() != credentials.Device.Principal() || peer.Device.Tenant() != local.Device.Tenant() || peer.Device.Principal() == local.Device.Principal() {
		return time.Time{}, meshid.Device{}, errors.New("net app session identity changed")
	}
	expires := local.NotAfter
	if peer.NotAfter.Before(expires) {
		expires = peer.NotAfter
	}
	// Legacy wall-time validity has the same UTC endpoint, but also retain
	// its initial monotonic duration as the old one-shot expiry timer did.
	deadline := started.Add(expires.Sub(started))
	if credentials.HasCertificateTimePolicy() {
		policy := credentials.CertificateTimePolicy()
		base := policy.BaseTime(policy.WallTime())
		deadline = started.Add(expires.Sub(base))
	}
	if !deadline.After(time.Now()) {
		return time.Time{}, meshid.Device{}, errors.New("net app credentials expired")
	}
	return deadline, peer.Device, nil
}
