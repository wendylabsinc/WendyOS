package meshsharing

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

var errControlCredentialInvalid = errors.New("gateway control credential revalidation failed")

// A certificate clock is never a socket deadline. Keep a monotonic resource
// bound, and only shorten it when the current local policy becomes stricter.
type controlValidity struct {
	mu       sync.Mutex
	deadline time.Time
	check    func() (time.Time, error)
	failure  error
}

func (v *controlValidity) current() (time.Time, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failure != nil {
		return v.deadline, v.failure
	}
	next, err := v.check()
	now := time.Now()
	if err == nil && !now.Before(next) {
		err = errors.New("credential lifetime ended")
	}
	if err != nil {
		v.failure = errors.Join(errControlCredentialInvalid, err)
		if now.Before(v.deadline) {
			v.deadline = now
		}
		return v.deadline, v.failure
	}
	if next.Before(v.deadline) {
		v.deadline = next
	}
	if !now.Before(v.deadline) {
		v.failure = errors.Join(errControlCredentialInvalid, errors.New("initial credential lifetime ended"))
	}
	return v.deadline, v.failure
}
func (v *controlValidity) failed() error { v.mu.Lock(); defer v.mu.Unlock(); return v.failure }

// A policy watcher is necessary even with no application I/O: direct time sync
// can end boot NotBefore grace or shorten either endpoint's selected path.
func newControlExpiryConn(conn net.Conn, expires time.Time, creds *localmesh.Credentials, state tls.ConnectionState, peer localmesh.Identity, onClose func()) *expiryConn {
	c := &expiryConn{Conn: conn, expires: expires, onClose: onClose}
	if !creds.HasCertificateTimePolicy() {
		return c
	}
	c.validity = &controlValidity{deadline: expires, check: func() (time.Time, error) {
		current, err := controlPeer(creds, state)
		if err != nil {
			return time.Time{}, err
		}
		if current.Device.IsOperator() {
			// TLS revalidates the signing principal, not the installation
			// UUID bound by the authenticated control exchange and source
			// address. Preserve that session's instance before comparing.
			current.Device, err = current.Device.WithInstance(peer.Device.UUID())
			if err != nil {
				return time.Time{}, err
			}
		}
		if current.Device != peer.Device {
			return time.Time{}, errors.New("gateway control peer changed")
		}
		return controlExpiry(creds, current)
	}}
	c.stop = make(chan struct{})
	c.watchDone = make(chan struct{})
	go c.watchValidity()
	return c
}

func (c *expiryConn) currentExpiry() time.Time {
	if c.validity == nil {
		return c.expires
	}
	expiry, _ := c.validity.current()
	return expiry
}
func (c *expiryConn) checkValidity() error {
	if c.validity == nil {
		return nil
	}
	_, err := c.validity.current()
	if err != nil {
		_ = c.Close()
	}
	return err
}
func (c *expiryConn) watchValidity() {
	defer close(c.watchDone)
	for {
		expiry, err := c.validity.current()
		if err != nil {
			_ = c.retire()
			return
		}
		timer := time.NewTimer(min(time.Second, time.Until(expiry)))
		select {
		case <-c.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (c *expiryConn) retire() error {
	c.closeOnce.Do(func() {
		if c.stop != nil {
			close(c.stop)
		}
		if c.onClose != nil {
			c.onClose()
		}
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}
func (c *expiryConn) Close() error {
	err := c.retire()
	if c.watchDone != nil {
		<-c.watchDone
	}
	return err
}
func (c *expiryConn) Read(p []byte) (int, error) {
	if err := c.checkValidity(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if err != nil && c.validity != nil {
		err = errors.Join(err, c.validity.failed())
	}
	return n, err
}
func (c *expiryConn) Write(p []byte) (int, error) {
	if err := c.checkValidity(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if err != nil && c.validity != nil {
		err = errors.Join(err, c.validity.failed())
	}
	return n, err
}
