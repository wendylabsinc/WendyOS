package backend

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const (
	appTunnelSetupTimeout = 10 * time.Second
	appTunnelRetryDelay   = 5 * time.Second
)

type appTunnelRenewal struct {
	done chan struct{}
	err  error
}

type appTunnelTransport struct {
	agent       *grpcclient.AgentConnection
	expiry      time.Time
	stopClosing func() bool // Set only for a connection owned by this forward.
	users       int         // Includes dials in progress and open streams.
}

func (t *appTunnelTransport) closeOwned() {
	if t.stopClosing != nil && t.stopClosing() {
		_ = t.agent.Close()
	}
}

// App forwarders can outlive the certificate in their original connection.
// Re-enter the existing authenticated connect path before expiry.
func renewingAppTunnel(ctx context.Context, selector string, initial *grpcclient.AgentConnection, connect func(context.Context, string) (*grpcclient.AgentConnection, error), now func() time.Time) func(context.Context, int) (net.Conn, error) {
	var mu sync.Mutex
	current := &appTunnelTransport{agent: initial, expiry: appTunnelExpiry(initial)}
	var pending *appTunnelRenewal
	var retryAfter time.Time
	var retryErr error

	renew := func(attempt *appTunnelRenewal) {
		// Setup belongs to the forward and retains its original account scope.
		setup, cancel := context.WithTimeout(ctx, appTunnelSetupTimeout)
		defer cancel()
		next, err := connect(setup, selector)
		expiry := appTunnelExpiry(next)
		var retired *appTunnelTransport
		keepNext := false
		mu.Lock()
		at := now()
		if err == nil {
			err = setup.Err()
		}
		if err == nil {
			switch {
			case next == nil || next.RegistryDialer == nil:
				err = errors.New("device has no TCP tunnel transport")
			case !at.Before(expiry):
				err = errors.New("Cloud app tunnel setup returned no unexpired operator certificate")
			case current.agent.CertInfo != nil && current.agent.CertInfo.PemCertificate == next.CertInfo.PemCertificate:
				// The same leaf came back. Keep the existing transport.
			default:
				keepNext = true
				if current.users == 0 {
					retired = current
				}
				current = &appTunnelTransport{
					agent: next, expiry: expiry,
					stopClosing: context.AfterFunc(ctx, func() { _ = next.Close() }),
				}
			}
		}
		// An unchanged but valid leaf is a supported fallback. Avoid repeating
		// setup for every browser socket, and allow another attempt at expiry.
		retryAfter = at.Add(appTunnelRetryDelay)
		if at.Before(current.expiry) && current.expiry.Before(retryAfter) {
			retryAfter = current.expiry
		}
		retryErr = err
		pending = nil
		attempt.err = err
		close(attempt.done)
		mu.Unlock()
		if retired != nil {
			retired.closeOwned()
		}
		if !keepNext && next != nil {
			_ = next.Close()
		}
	}

	acquire := func(request context.Context) (*appTunnelTransport, error) {
		for {
			mu.Lock()
			if err := ctx.Err(); err != nil {
				mu.Unlock()
				return nil, err
			}
			if err := request.Err(); err != nil {
				mu.Unlock()
				return nil, err
			}
			at := now()
			due := appTunnelCertificateDue(current.expiry, at)
			if !due || at.Before(retryAfter) {
				if due && retryErr != nil {
					mu.Unlock()
					return nil, retryErr
				}
				transport := current
				transport.users++
				mu.Unlock()
				return transport, nil
			}
			if pending == nil {
				pending = &appTunnelRenewal{done: make(chan struct{})}
				go renew(pending)
			}
			attempt := pending
			mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-request.Done():
				return nil, request.Err()
			case <-attempt.done:
				if attempt.err != nil {
					return nil, attempt.err
				}
			}
		}
	}

	return func(request context.Context, port int) (net.Conn, error) {
		transport, err := acquire(request)
		if err != nil {
			return nil, err
		}
		release := func() {
			mu.Lock()
			transport.users--
			drained := transport != current && transport.users == 0
			mu.Unlock()
			if drained {
				transport.closeOwned()
			}
		}
		remote, err := transport.agent.RegistryDialer(request, port)
		if err != nil {
			if remote != nil {
				_ = remote.Close()
			}
			release()
			return nil, err
		}
		if remote == nil {
			release()
			return nil, errors.New("Cloud app tunnel returned no TCP connection")
		}
		return withConnRelease(remote, release), nil
	}
}

func appTunnelExpiry(conn *grpcclient.AgentConnection) time.Time {
	if conn == nil || conn.CertInfo == nil {
		return time.Time{}
	}
	cert, err := certs.ParseStoredLeaf(conn.CertInfo.PemCertificate)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}

func appTunnelCertificateDue(expiry, now time.Time) bool {
	return !expiry.IsZero() && !now.Before(expiry.Add(-time.Minute))
}
